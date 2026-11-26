# Module `vpc`

Réseau privé (VPC) d'un projet étendu à ses VPS ([ADR 0019](../adr/0019-vpc-reseau-prive-vps.md)).
Un VPS rattaché joint les conteneurs, services et bases du projet, et
inversement, par une adresse privée fixe et un nom en `.internal`.

État : **lot 1** — modèle, API, interfaces de mise en œuvre et leurs suites
de contrat, mise en œuvre de test (`memory`). **Aucune mise en œuvre réelle
n'est branchée** : rattacher répond `503 VPC_UNAVAILABLE` jusqu'au lot 2
(tunnel) et à Proxmox (réseau natif).

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

La réinstallation, la suspension et la résiliation d'un VPS retireront son
rattachement au lot 2, avec la mise en œuvre réelle.

## Endpoints

| Méthode | Chemin | Permissions | Description |
|---|---|---|---|
| `GET` | `/projects/:id/network/vpc` | `compute.vps.get` | Plage, VPS rattachés (adresse, nom `.internal`, état, `connected` vu par le réseau, dernier contact). Outil Copilot `get_project_vpc`. |
| `POST` | `/organizations/:id/compute/vps/:vpsID/vpc` | `compute.vps.network` (sur le projet du VPS) | Rattacher ; `enrollment` (jeton, expiration) seulement si le VPS doit s'inscrire. Copilot : renvoi vers l'écran (jeton montré une fois). |
| `DELETE` | `/organizations/:id/compute/vps/:vpsID/vpc` | `compute.vps.network` | Détacher (`204`). |
| `POST` | `/vpc/enroll` | publique (jeton d'inscription) | Inscription par le VPS ; route interne, hors de la doc publique. |

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
| `VPC_UNAVAILABLE` | 503 | aucune mise en œuvre pour ce placement (lot 1 : toujours) |
| `VPC_REGION_MISMATCH` | 409 | VPS et cluster du projet dans deux régions |
| `VPC_RANGE_EXHAUSTED` | 409 | plus de plage dans le cluster, ou plage du projet pleine |
| `VPS_NOT_READY` | 409 | VPS en création, suspendu… |
| `VPC_NOT_ATTACHED` | 404 | détacher un VPS non rattaché |
| `VPC_ENROLLMENT_INVALID` | 400 | jeton inconnu, expiré ou déjà utilisé |
| `VPC_ENROLLMENT_NOT_NEEDED` | 409 | inscription pour un VPS qui n'en a pas besoin |
| `INVALID_INPUT` | 400 | clé publique ou règle de pare-feu invalide |
| `FIREWALL_TOO_MANY_RULES` | 400 | plus de règles que la mise en œuvre n'en permet |

## Stockage (migration 000083)

- `vpc_networks` : `project_id` (clé), `cluster_id`, `cidr` (/24 dans `10.248.0.0/13`).
- `vpc_attachments` : `vps_id` (clé), `project_id`, `address` (unique par
  projet), `backend`, `state` (`pending` | `attached`), `public_key`, dates.
- `vpc_enrollments` : `token_hash` (32 octets, unique), `expires_at`, `used_at`.

Suppression en cascade avec le projet, le VPS ou le rattachement.

## Journal

Rattacher et détacher passent par le journal d'audit comme toute route qui
modifie (auteur, ressource du projet du VPS).
