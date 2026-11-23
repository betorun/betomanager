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
