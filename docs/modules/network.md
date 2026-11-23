# Module `network`

Réseau privé d'un projet, vu par le client : qui peut joindre qui, avec quelles adresses. Lecture seule.

## Rôle

- Décrire le réseau **sans rien du système qui l'applique**. Aujourd'hui, les règles sont posées par des NetworkPolicy Kubernetes (chart `standard-app`, bases CloudNativePG) ; demain par le pare-feu d'un autre hébergeur (Proxmox, VPS reliés au projet). La console affiche ce modèle, jamais l'outil (marque blanche).
- Tout vient de la base (conteneurs, bases, domaines vérifiés, branchements, `wait_for`) : rien n'est lu sur le cluster, la page répond même si le cluster est muet.

## Endpoint

| Méthode | Chemin | Permissions | Description |
|---|---|---|---|
| `GET` | `/projects/:id/network` | `workloads.workloads.list` et `databases.databases.list` | Membres, liens déclarés, règles. Outil Copilot `get_project_network` (lecture). |

Réponse (`domain.ProjectNetwork`) :

- `members` : conteneurs (`web`, `service`, `worker`, `cron`) puis bases (`database`), triés par sorte puis nom.
  - `internal` : adresse dans le réseau du projet, nom court (`http://api`, `cache:6379`, `db-main-rw:5432`) ; absente pour un worker ou un cron (n'écoute rien).
  - `public` : adresses joignables depuis Internet, seulement pour un conteneur `web` : adresse automatique (`automatic`) et domaines **vérifiés** (`domain`, https sauf `kubernetes.custom_domain_insecure`). Un service ou une base n'est jamais public.
- `links` : liens **déclarés** (le réseau permet aussi tout lien non déclaré entre membres) : base branchée (`database`, avec la variable qui porte l'adresse, jamais sa valeur) ; attente au démarrage (`waits_for`, depuis la spec `wait_for`, forme longue `nom.espace.svc` ramenée au nom). Une base branchée n'a qu'un lien. Une adresse attendue qui ne correspond à aucun membre est ignorée.
- `rules` : la politique v1, la même pour tout projet ([domain/network.go](../../internal/modules/network/domain/network.go), `PolicyRules`) :

| Code | De | Vers | Ports | Permis |
|---|---|---|---|---|
| `project_internal` | projet | projet | tous | oui |
| `project_to_databases` | projet | bases | 5432 | oui |
| `internet_to_public_web` | Internet | web public | 80/443 | oui |
| `internet_to_internal` | Internet | projet | tous | non |
| `other_projects` | autres projets | projet | tous | non |
| `egress_internet` | projet | Internet | 80, 443, 465, 587, 3306, 5432, 6379, 27017 (`EgressPorts`, jamais le 25) | oui |
| `egress_private` | projet | adresses privées | tous | non |

## Invariants

1. **Toute implémentation applique `PolicyRules`.** Les ports sortants (`EgressPorts`) sont lus par le chart des conteneurs : une seule liste. Changer une règle = changer les deux côtés (chart et bases aujourd'hui, l'autre hébergeur demain) dans le même lot ; `TestPolicyRules_Stable` casse exprès à chaque changement.
2. Aucun nom d'espace Kubernetes, de cluster ou de fournisseur dans la réponse.
3. Aucune valeur de variable (l'adresse d'une base branchée est secrète).

## Fichiers clés

- [domain/network.go](../../internal/modules/network/domain/network.go) — modèle et politique
- [service/service.go](../../internal/modules/network/service/service.go) — lecture et liens
- [network_wiring.go](../../internal/infrastructure/router/network_wiring.go) — route
- Application aujourd'hui : [network-policy.yaml](../../internal/infrastructure/helm/charts/standard-app/templates/network-policy.yaml), `networkPolicy` de [cnpg.go](../../internal/modules/databases/repository/cnpg.go)

## IP de sortie dédiée (module `egress`, ADR 0014)

Un projet peut avoir une IP propre par laquelle sortent tous ses conteneurs (à faire autoriser chez un partenaire). Décision et découpage : [ADR 0014](../adr/0014-ip-sortie-dediee.md).

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| `GET` | `/projects/:id/network/egress` | `network.egressAddresses.get` (lecteur) | `address` (vide : aucune), `offered` (la région en propose), `available` (une est libre maintenant). Outil Copilot `get_egress_ip`. |
| `POST` | `/projects/:id/network/egress` | `network.egressAddresses.manage` (éditeur) | Une IP libre du cluster du projet, puis la règle posée ; règle refusée : l'IP revient dans la réserve. `409 EGRESS_NOT_OFFERED`, `EGRESS_NONE_FREE`, `EGRESS_ALREADY_ASSIGNED` ; `503 EGRESS_GATEWAY_MISSING`. |
| `DELETE` | `/projects/:id/network/egress` | `network.egressAddresses.manage` | Règle retirée, IP rendue (la facturation s'arrête). |
| `GET` / `POST` | `/admin/clusters/:clusterID/egress-addresses` | `platform.clusters.list` / `.manage` | Réserve du cluster (avec le projet qui tient chaque IP) ; ajout d'une IPv4 publique **déjà montée** sur l'interface du nœud passerelle (`gateway_node`). |
| `DELETE` | `/admin/clusters/:clusterID/egress-addresses/:addressID` | `platform.clusters.manage` | Retrait d'une IP libre (`409 EGRESS_ADDRESS_IN_USE` sinon). |

- **Passerelle du cluster** : `clusters.egress_gateway` (`PATCH /admin/clusters/:clusterID`, `egress_gateway`) : vide = pas d'IP dédiée, `cilium` = règles `CiliumEgressGatewayPolicy` (une par projet, `betorun-egress-<namespace>`, tous les pods du namespace, destinations privées exclues, nœud passerelle et IP de la réserve).
- **Réserve** : table `egress_addresses` (migration 000072) ; deux demandes simultanées n'obtiennent jamais la même IP (`FOR UPDATE SKIP LOCKED`), un projet n'en a qu'une (index unique).
- **Facturation** : compteur horaire `egress_ip`, prix mensuel `egress_ip_monthly` de la grille (back-office, Grilles de prix), ligne « IP de sortie dédiées » de la facture.
- **Projet supprimé** : le worker rend ses IP toutes les 5 minutes (`Service.Sweep`) ; la facturation ne compte que les projets vivants.
- **Compte plateforme** : droits sur `cilium.io/ciliumegressgatewaypolicies` (`deploy/k8s/rbac.yaml`, `PlatformRules`).
