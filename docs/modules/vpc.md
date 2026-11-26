# Module `vpc`

Réseau privé (VPC) d'un projet étendu à ses VPS ([ADR 0019](../adr/0019-vpc-reseau-prive-vps.md)).
Un VPS rattaché joint les conteneurs, services et bases du projet, et
inversement, par une adresse privée fixe et un nom en `.internal`.

État : **lots 1 et 2**. Lot 1 : modèle, API, interfaces de mise en œuvre
et leurs suites de contrat, mise en œuvre de test (`memory`). Lot 2 : mise
en œuvre `tunnel` (passerelle par projet), inscription au premier démarrage,
commande pour un VPS existant, suivi de la réinstallation, de la suspension
et de la résiliation. Sans configuration (`vpc.gateway_image`, URL publique
de l'API), rattacher répond `503 VPC_UNAVAILABLE`. Lot 3 : noms `.internal`,
VPS ↔ VPS, sens conteneurs → VPS, page Réseau, CLI.

## Indépendant de l'hébergeur

L'API, les codes d'erreur et la console ne nomment jamais l'outil ni
l'hébergeur. La mise en œuvre passe par deux interfaces du domaine
([domain/vpc.go](../../internal/modules/vpc/domain/vpc.go)) :

- `VPCBackend` : `Handles(Placement)`, `Capabilities()`, `Ensure`, `Attach`,
  `Enroll`, `Detach`, `Status`, `Teardown`. Choisie au rattachement par le
  placement (hébergeur du VPS, cluster du projet et son hébergeur) : la
  première qui sait le servir. Son nom est gardé sur le rattachement
  (`vpc_attachments.backend`), jamais rendu par l'API.
- `FirewallProvider` : `Capabilities()` (règles du client au plus, avec ou
  sans état, IPv6), `Apply` (remplace tout), `Get`. Utilisé au lot 4.

Toute mise en œuvre passe les suites de [vpctest](../../internal/modules/vpc/vpctest/contract.go)
(`RunBackendContract`, `RunFirewallContract`) : idempotence, isolement de
deux VPC (même plage dans deux clusters comprise), détachement effectif même
d'un VPS inconnu, aucune clé privée dans la configuration rendue au VPS,
règles réservées jamais posées par le client, limites refusées sans rien
changer. La mise en œuvre `memory` les passe sous ses deux formes (avec
inscription, comme un tunnel ; sans, comme un réseau natif).

## Adresses et noms

- **Plage** : un /24 par VPC dans `10.248.0.0/13`, créé au premier VPS
  rattaché, unique dans le cluster du projet (`vpc_networks`, contrainte
  `UNIQUE (cluster_id, cidr)`) ; gardé par le projet quand le dernier VPS
  part (un VPS rattaché plus tard retrouve la même).
- **Adresse d'un VPS** : la première libre de la plage, ni `.0`, ni `.1`
  (plateforme), ni `.255` ; libérée au détachement, redonnée ensuite.
- **Attribution** : une transaction, verrous consultatifs par projet et par
  cluster (rattachements simultanés : adresses toujours distinctes, testé).
- **Nom** : `<nom-du-vps>.internal` (minuscules).

## Rattacher, inscrire, détacher

- **Rattacher** : VPS en marche ou arrêté (`409 VPS_NOT_READY` sinon), même
  région que le cluster du projet quand les deux sont connues
  (`409 VPC_REGION_MISMATCH`). Idempotent : l'adresse ne change pas.
  - mise en œuvre sans inscription : le VPS est relié (`attached`) ;
  - avec inscription : `pending`, et un **jeton d'inscription** est rendu
    une seule fois (`bvpc_…`, 32 octets aléatoires, 30 min, usage unique ;
    seule son empreinte SHA-256 est gardée). Un nouveau rattachement donne
    un nouveau jeton et annule l'ancien.
- **Inscrire** : le VPS envoie son jeton et la clé publique **générée sur
  lui** ; la clé est contrôlée avant que le jeton soit consommé ; le jeton est
  consommé en une seule requête (deux inscriptions simultanées : une seule
  réussit, testé). Réponse : adresse, plage, suffixe des noms et
  configuration propre à la mise en œuvre (jamais de secret).
- **Détacher** : effet immédiat côté mise en œuvre puis en base, même VPS
  injoignable ; le dernier VPS parti, le réseau est démonté (`Teardown`).

## Cycle de vie du VPS (lot 2)

La base est la source de vérité ; le réseau la suit à chaque changement d'un
VPS (événement compute, en tâche de fond), à l'inscription, et par une
réconciliation du worker toutes les 2 minutes (`DefaultReconcileEvery`) qui
retire aussi les membres que la base ne connaît plus.

| Moment | Effet |
|---|---|
| **Commande** (`private_network: true`) | refus avant tout débit si le réseau est impossible (`VPC_UNAVAILABLE`, `VPC_REGION_MISMATCH`) ; choix retenu sur le VPS (`compute_vps.private_network`). |
| **Premier démarrage** (création) | le mode compute prépare le rattachement **en base seulement** (aucun accès aux clusters depuis ce process) : adresse, jeton de 2 h (`InstallEnrollmentTTL`), script d'inscription ajouté après la règle du port 25, en sous-shell (son échec n'arrête rien). Réseau impossible : l'installation continue, le choix est retiré. |
| **Rattacher un VPS existant** | jeton de 30 min et `command` : `curl -fsS <API>/vpc/enroll.sh \| sudo sh -s -- bvpc_…`, à lancer en root sur le VPS. Choix retenu sur le VPS. |
| **Réinstallation** | à la demande (API) : clé de l'ancien système retirée tout de suite (un VPS réinstallé parce que compromis ne garde pas le réseau), adresse gardée ; au premier démarrage du nouveau système, nouvelle inscription par script. |
| **Suspension** (impayé, organisation, abus) | accès coupé côté réseau, rattachement **gardé** (adresse et clé) ; rendu à la reprise, sans rien faire sur le VPS. |
| **Résiliation**, création en échec | rattachement retiré (le dernier VPS parti, la passerelle est démontée). |
| **Détacher** | accès coupé puis rattachement retiré ; choix retiré du VPS. |

**Inscription** : le script installe les outils (Debian, Ubuntu), génère la
clé **sur le VPS**, envoie la clé publique avec le jeton (`POST /vpc/enroll`,
en IPv4), écrit la configuration et active l'interface `vpc0`. L'API refuse
l'inscription d'un VPS suspendu (`VPS_NOT_READY`), et un jeton présenté
depuis une adresse publique qui n'est pas celle du VPS
(une adresse privée vient d'un mandataire : non comparée) ; IP du VPS pas
encore relevée chez le fournisseur : celle de la requête est retenue.

## Mise en œuvre `tunnel` (fournisseur sans réseau privé)

Paquets : [backend/tunnel](../../internal/modules/vpc/backend/tunnel) (API),
[internal/vpcgateway](../../internal/vpcgateway) (format partagé, agent),
binaire [cmd/vpc-gateway](../../cmd/vpc-gateway), image
`thekrauss/vpc-gateway:<sha>` ([docker/vpc-gateway.Dockerfile](../../docker/vpc-gateway.Dockerfile),
construite par la CI). Choisie pour un VPS `ovh` quand l'image est configurée.

Ressources posées dans l'espace du projet, toutes nommées
`betorun-private-network-gateway` (plus long qu'un nom de conteneur du
client, 30 au plus : jamais en collision avec ses ressources ni ses pods),
au premier VPS rattaché et retirées au dernier, sous le verrou des
rattachements du projet (un VPS rattaché entre-temps garde sa passerelle) :

- **Secret** : clé privée de la passerelle et jeton de son état, générés
  par l'API, jamais rendus.
- **ConfigMap** (`gateway.json`) : plage, adresse de la passerelle (`.1`),
  plages joignables, pairs (VPS, adresse, clé publique, IP publique).
- **Deployment** (1 réplique, `Recreate`) : root dans le conteneur mais
  seulement `NET_ADMIN` (tout le reste retiré), système de fichiers en
  lecture seule, seccomp par défaut, pas de jeton de compte de service ;
  10m/32Mi demandés, 200m/64Mi au plus. Jamais pris sur la limite du
  client : la limite de l'API ne compte que ses conteneurs, et le quota
  Kubernetes de l'espace du projet ajoute une réserve égale (constantes
  `vpcgateway.Limit*`, appliquée à chaque mise à jour des limites du
  projet).
  Étiquettes de membre du projet (`app.kubernetes.io/part-of`,
  `km-project-id`) : les NetworkPolicy du projet la laissent joindre les
  conteneurs et les bases. L'agent applique la configuration (`wg
  syncconf`, toutes les 5 s ; le ConfigMap monté suit en une minute au plus)
  et filtre : un VPS ne joint que les plages des pods et services
  (`vpc.cluster_routes`), jamais Internet par la passerelle, ni la
  passerelle elle-même, ni (lot 3) un autre VPS.
- **Service** `NodePort` UDP, `externalTrafficPolicy: Local` : port alloué
  par le cluster (unique), source préservée.
- **NetworkPolicy** : UDP admis des seules IP publiques des VPS rattachés
  (aucune : fermé) ; état (`GET /status`, port 8080, jeton du Secret exigé :
  le port est joignable du cluster) lu par l'API via le mandataire de l'API
  Kubernetes (droit `pods/proxy` du rôle de la plateforme) ; sortie vers
  l'espace du projet, le DNS du cluster et les VPS.

Réponse de l'inscription (`config`) : `endpoint` (point d'entrée du cluster
`ingress_target` et port), `peer_public_key`, `allowed_ips` (plage du VPC et
plages du cluster), `keepalive`. État d'un membre (`connected`) : poignée de
main de moins de 3 minutes.

Trafic : à chaque synchronisation (au moins toutes les 2 min), les
compteurs de chaque lien (octets envoyés et reçus par le VPS, lus avec
l'état) sont comparés au relevé précédent ; l'écart s'ajoute au mois en
cours du projet. Un compteur plus bas qu'avant (lien refait, passerelle
redémarrée) compte depuis zéro. Mesure pour l'équipe seulement : **rien
n'est facturé** (prix : décision du propriétaire). Le trafic d'un lien
refait puis déjà plus haut que l'ancien relevé dans le même intervalle est
sous-compté : négligeable à 2 min.

Prérequis d'exploitation :

- rôle de la plateforme à jour sur chaque cluster (`pods/proxy` en lecture,
  ajouté au lot 2) : `kubectl apply -f deploy/k8s/rbac.yaml` ou « Préparer
  le cluster » ; sans lui, l'état du lien est inconnu (`connected` absent) ;
- NetworkPolicy appliquées par le cluster (Cilium) : c'est elles qui
  limitent la passerelle à l'espace du projet ;

- module noyau du tunnel sur les nœuds (noyaux Linux ≥ 5.6) ;
- ports UDP `NodePort` (30000-32767) ouverts vers le nœud d'entrée du
  cluster (pare-feu de l'hébergeur et de la machine) ;
- avec plusieurs nœuds, la passerelle doit tourner sur le nœud
  `ingress_target` (source préservée) : à traiter avant le premier cluster
  multi-nœud.

Configuration :

| Clé | Variable | Rôle |
|---|---|---|
| `vpc.gateway_image` | `KUBEMANAGER_VPC_GATEWAY_IMAGE` | image de la passerelle ; vide : indisponible |
| `vpc.public_api_url` | `KUBEMANAGER_VPC_PUBLIC_API_URL` | URL publique de l'API (`…/kmanager/v1`) appelée par les VPS ; vide : déduite de `iam.sts_issuer` |
| `vpc.cluster_routes` | `KUBEMANAGER_VPC_CLUSTER_ROUTES` | plages joignables (défaut `10.42.0.0/16,10.43.0.0/16`) |

## Endpoints

| Méthode | Chemin | Permissions | Description |
|---|---|---|---|
| `GET` | `/projects/:id/network/vpc` | `compute.vps.get` | Plage, VPS rattachés (adresse, nom `.internal`, état, `connected` vu par le réseau, dernier contact). Outil Copilot `get_project_vpc`. |
| `POST` | `/organizations/:id/compute/vps/:vpsID/vpc` | `compute.vps.network` (sur le projet du VPS) | Rattacher ; `enrollment` (`token`, `expires_at`, `command` à lancer en root) seulement si le VPS doit s'inscrire. Copilot : renvoi vers l'écran (jeton montré une fois). |
| `DELETE` | `/organizations/:id/compute/vps/:vpsID/vpc` | `compute.vps.network` | Détacher (`204`). |
| `GET` | `/admin/vpc/traffic?month=AAAA-MM` | `platform.compute.get` | Trafic du réseau privé par projet sur un mois (mois en cours par défaut) : projet, organisation, `from_vps_bytes`, `to_vps_bytes`, totaux. Mesure seulement. Outil de l'Opérateur `get_private_network_traffic`. |
| `POST` | `/vpc/enroll` | publique (jeton d'inscription) | Inscription par le VPS ; route interne, hors de la doc publique. |
| `GET` | `/vpc/enroll.sh` | publique | Script d'inscription, sans jeton (donné en argument) ; `404` sans mise en œuvre qui en a besoin. Route interne. |

À la commande d'un VPS (`POST /organizations/:id/compute/vps`), le champ
`private_network` (booléen, défaut faux ; la console le coche par défaut)
demande le rattachement dès le premier démarrage ; il est rendu sur le VPS.

Les routes des VPS n'existent que si les VPS sont vendus sur le déploiement
(contrôleur compute présent). `GET /projects/:id/network` montre aussi les VPS
rattachés (`kind: "vps"`, `status` : `pending` ou `attached`).

Réponse de `GET /projects/:id/network/vpc` :

```json
{
  "enabled": true,
  "range": "10.248.0.0/24",
  "name_suffix": ".internal",
  "members": [
    {"vps_id": "…", "name": "web", "address": "10.248.0.2", "internal_name": "web.internal",
     "state": "attached", "connected": true, "attached_at": "…", "last_contact_at": "…"}
  ]
}
```

## Erreurs

| Code | HTTP | Cas |
|---|---|---|
| `VPC_UNAVAILABLE` | 503 | aucune mise en œuvre pour ce placement (ou non configurée), aussi à la commande avec `private_network` |
| `VPC_REGION_MISMATCH` | 409 | VPS et cluster du projet dans deux régions |
| `VPC_RANGE_EXHAUSTED` | 409 | plus de plage dans le cluster, ou plage du projet pleine |
| `VPS_NOT_READY` | 409 | VPS en création, suspendu… |
| `VPC_NOT_ATTACHED` | 404 | détacher un VPS non rattaché |
| `VPC_ENROLLMENT_INVALID` | 400 | jeton inconnu, expiré ou déjà utilisé |
| `VPC_ENROLLMENT_NOT_NEEDED` | 409 | inscription pour un VPS qui n'en a pas besoin |
| `INVALID_INPUT` | 400 | clé publique ou règle de pare-feu invalide |
| `FIREWALL_TOO_MANY_RULES` | 400 | plus de règles que la mise en œuvre n'en permet |

## Stockage (migrations 000083 à 000085)

- `vpc_networks` : `project_id` (clé), `cluster_id`, `cidr` (/24 dans `10.248.0.0/13`).
- `vpc_attachments` : `vps_id` (clé), `project_id`, `address` (unique par
  projet), `backend`, `state` (`pending` | `attached`), `public_key`, dates.
- `vpc_enrollments` : `token_hash` (32 octets, unique), `expires_at`, `used_at`.

- `compute_vps.private_network` (000084) : réseau privé voulu par le client,
  relu à chaque installation.
- `vpc_traffic_peers` (000085) : derniers compteurs lus par VPS ;
  `vpc_traffic_monthly` : cumul par projet et par mois (premier jour, UTC).

Suppression en cascade avec le projet, le VPS ou le rattachement.

## Journal

Rattacher et détacher passent par le journal d'audit comme toute route qui
modifie (auteur, ressource du projet du VPS).
