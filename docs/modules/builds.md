# Module `builds`

> Construction d'images depuis le code (archive `.tar.gz`, dépôt Git public ou dépôt GitHub relié) par un Job BuildKit, publication dans le registre de la plateforme, ménage et quota des images.

## Modèle

Un build = une ligne `builds` (migrations 000017, 000021, 000025) et un workflow Temporal `build-<id>`. L'image est publiée sous `<registry_host>/<projet>/<nom>:b-<12 premiers caractères de l'id>` ; le nom (2 à 63 caractères, minuscules, chiffres, tirets) est souvent celui du conteneur. Le build ne déploie rien par lui-même, sauf sur la voie GitHub (`deploy_workload_id`) : le serveur met alors le conteneur à jour en cas de réussite.

- **États** : `QUEUED` → `BUILDING` → `SUCCEEDED`, `FAILED` ou `CANCELLED`.
- **Causes d'échec** (`failure`, codes stables) : `BUILD_FAILED`, `TIMEOUT`, `DISK_LIMIT`, `NO_BUILD_PLAN`, `GIT_ACCESS_LOST`, `TOO_MANY_BUILDS`, `IMAGE_QUOTA`, `SUPERSEDED`, `INTERNAL`.
- **Origines** (`source_type`) : `upload`, `git` (dépôt public), `github` (installation GitHub, dépôt privé possible, commit exact).
- **Déploiement enchaîné** (`deploy_state`, voie GitHub) : `PENDING`, `DEPLOYED`, `BUSY`, `FAILED`, `STOPPED`, `GONE`, `SUPERSEDED`, `NOT_BUILT`.
- `image_size` (taille compressée lue au registre) et `image_pruned_at` (image retirée du registre ; le build et son journal restent).

## Endpoints HTTP

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/projects/:id/builds` | `workloads.builds.list` | Builds récents (`?name=` pour une seule image) |
| POST | `/projects/:id/builds` | `workloads.builds.create` | Build d'un dépôt Git public (`name`, `git_url`, `git_ref`, `context_dir`, `dockerfile`) — 202 |
| POST | `/projects/:id/builds/upload` | `workloads.builds.create` | Build d'une archive (multipart : `archive`, `name`, `dockerfile`) — 202 |
| GET | `/projects/:id/builds/:buildID` | `workloads.builds.get` | Détails d'un build |
| GET | `/projects/:id/builds/:buildID/logs` | `workloads.builds.get` | Journal en texte brut, en flux (`?follow=true`) |
| POST | `/projects/:id/builds/:buildID/cancel` | `workloads.builds.cancel` | Annule un build en cours — 204 |
| DELETE | `/projects/:id/builds/:buildID/image` | `workloads.builds.delete` | Retire l'image du registre (le build et son journal restent) — 204 |
| GET | `/builds/:buildID/source` | aucune (URL signée) | Interne : archive téléchargée par le Job |
| GET | `/registry/token` | aucune (Basic auth du registre) | Interne : jeton du registre (Docker token auth) |

Rôles : lecteur = `list`, `get` ; développeur = + `create`, `cancel` ; éditeur = + `delete` ([roles.go](../../internal/iam/roles.go)). Les builds GitHub sont déclenchés par le module GitHub (push ou « déployer maintenant »), pas par ces routes.

## Déroulé

1. **Soumission** ([service.go](../../internal/modules/builds/service/service.go)) : validation du nom, du chemin du Dockerfile (relatif, sans `..`) et de l'URL Git (https, sans identifiants ni IP ni hôte `.local`/`.internal`, `.git` ajouté). Refus `TOO_MANY_BUILDS` (429) au-delà de `max_concurrent` builds actifs, puis contrôle du quota d'images (`IMAGE_QUOTA`, 409). Une archive doit commencer par l'en-tête gzip ; elle est rangée dans le stockage d'objets (`builds/<projet>/<build>/source.tar.gz`). Sur la voie GitHub, les builds encore en cours du même conteneur sont annulés d'abord (`SUPERSEDED`), et un refus pour quota laisse une trace `FAILED` (un push n'a personne à qui répondre).
2. **Workflow** ([workflows.go](../../internal/modules/builds/workflows/workflows.go)) : `MarkBuilding`, `RunBuild` (crée l'espace `betorun-builds`, sa politique réseau et le Job, puis le surveille), `Finish` sur un contexte détaché quelle que soit l'issue, puis `DeployBuilt` si le build vient de GitHub (environ dix minutes d'essais si un déploiement occupe le conteneur, sinon `BUSY`).
3. **Job BuildKit** ([job.go](../../internal/modules/builds/activities/job.go), [build.sh](../../internal/modules/builds/activities/build.sh)) : image BuildKit sans root, utilisateur 1000, sans jeton Kubernetes, une tentative, `activeDeadlineSeconds` = `builds.timeout`, disque éphémère limité. La source vient de l'URL signée (archive) ou de `git clone --depth 1` (commit exact via `fetch` sur la voie GitHub ; jeton de clonage passé par un Secret puis un en-tête HTTP, jamais dans l'URL). Sans Dockerfile, [detect.sh](../../internal/modules/builds/activities/detect.sh) en génère un (Node/Next/Vite…, Python Django/FastAPI/Flask/Streamlit, site statique nginx), écoute sur 8080 ; sinon `NO_BUILD_PLAN`. Le cache est poussé dans le registre (`--export-cache`) et réimporté dès qu'un build précédent de la même image a réussi. Le digest et le type détecté reviennent par le message de fin du pod.
4. **Fin** ([activities.go](../../internal/modules/builds/activities/activities.go)) : le journal (fin gardée, 32 Mio au plus) est écrit dans le stockage d'objets avant l'état final ; taille de l'image et ménage ; alerte e-mail sur échec ; suppression du Job, des Secrets `build-auth-<id>` et `build-git-<id>` et de l'archive.

Le journal se lit sur le pod tant que le build tourne (attente jusqu'à 2 min du démarrage avec `follow`), puis dans le stockage d'objets.

## Registre de la plateforme

Le registre délègue son authentification à l'API (Docker token auth, [registry.go](../../internal/modules/builds/registry/registry.go)). Deux identifiants dérivés de `builds.secret` par HMAC, sans stockage :

- `build.<projet>.<expiration>` : lecture et écriture, valable le temps du build (délai + 1 min), monté dans le Job ;
- `pull.<projet>` : lecture, placé dans le Secret de pull `betorun-registry` du namespace du projet (voir [registries.md](registries.md)).

`GET /registry/token` vérifie l'identifiant et renvoie un JWT ES256 (5 min, chaîne `x5c`) limité aux dépôts `<projet>/…` ; un scope hors du projet n'est simplement pas accordé. Côté déploiement, une image du registre de la plateforme n'est déployable que par son projet (`FOREIGN_IMAGE`, 403), sinon un nœud ayant l'image en cache la lancerait. L'archive est servie par `/builds/:id/source?expires=&sig=` (signature HMAC propre aux sources), et seulement tant que le build n'est pas terminé.

## Ménage, quota et suppression d'une image

Décisions : [ADR 0010](../adr/0010-registre-menage-quota.md). Code : [images.go](../../internal/modules/builds/images/images.go), [client.go](../../internal/modules/builds/registry/client.go).

- **Après un build réussi** : taille lue au manifeste (index compris, couche commune comptée une fois), puis, par dépôt `<projet>/<nom>`, on garde les `keep_images` dernières images et toutes celles d'un conteneur du projet ; les autres sont supprimées du registre (`DELETE /v2/<dépôt>/manifests/<digest>`, jeton que l'API se délivre, sujet `platform`) et marquées `image_pruned_at`. Un digest encore porté par une image gardée n'est pas supprimé. Au mieux : un échec est journalisé, le build reste réussi.
- **Quota** : usage = somme des tailles, un digest compté une fois par dépôt. Atteint à la soumission, le projet est d'abord compacté (dernière image + images en ligne par dépôt) ; refus seulement si ça ne suffit pas. L'usage et le quota apparaissent dans la vue du projet.
- **Suppression à la demande** (`DELETE …/image`) : `NO_IMAGE` (409) si le build n'a pas réussi ou si l'image est déjà retirée ; `IMAGE_IN_USE` (409) si un conteneur du projet la fait tourner (par étiquette ou par digest). Si un autre build gardé porte le même digest, rien n'est effacé au registre : la ligne est seulement marquée.
- **Disque** : libéré par le garbage-collect du registre (`deploy/registry/gc.sh`), à planifier.

Une image construite se propage aussi aux conteneurs qui suivent celui qu'elle met à jour (`image_from`, module `workloads`, un seul niveau).

## Configuration

Section `builds` ([config.go](../../internal/core/configs/config.go)) : `enabled` (exige le stockage d'objets ; en production `sites.enabled`), `namespace` (`betorun-builds`), `buildkit_image` (`moby/buildkit:v0.23.2-rootless`), `registry_host`, `registry_insecure` (développement seulement), `registry_api_url` (défaut `https://<registry_host>`), `keep_images` (5), `project_image_quota` (`5Gi`, `"0"` = sans), `registry_service` (`betorun-registry`), `token_issuer` (`betorun`), `token_cert_file` / `token_key_file` (ECDSA P-256), `secret` (≥ 32 octets), `public_api_url` (API vue depuis les pods), `max_archive_bytes` (200 Mio), `max_concurrent` (2), `timeout` (30 min), `cpu_request`/`cpu_limit` (`250m`/`2`), `memory_request`/`memory_limit` (`512Mi`/`4Gi`), `disk_limit` (`10Gi`), `egress_allow_cidrs`, `runtime_class`, `node_selector`.

## Pièges

- **Changer `builds.secret`** invalide tous les Secrets de pull existants (et les URL signées des imports de base, qui partagent ce secret).
- **Réseau du Job** : DNS et Internet public seulement ; les plages privées (10/8, 172.16/12, 192.168/16, 100.64/10, 169.254/16, 127/8) sont refusées sauf `egress_allow_cidrs` (registre et API sur un réseau privé).
- **Isolation** : BuildKit sans root exige seccomp/AppArmor non confinés ; en production, prévoir `runtime_class` (gVisor, Kata) et des nœuds dédiés. Le code d'un Dockerfile peut lire l'identifiant de son build : il ne donne rien de plus que le projet et expire avec le build.
- **Annulation** : passe par les heartbeats Temporal (10 s) ; annuler un build terminé renvoie `BUILD_FINISHED` (409).
- Sans clé de jetons lisible côté worker, le ménage est désactivé (avertissement au démarrage) mais les builds fonctionnent.

## Fichiers clés

- [builds.go](../../internal/modules/builds/builds.go) — handlers (upload multipart, journal en flux, source, jeton du registre).
- [service/service.go](../../internal/modules/builds/service/service.go) — soumission, annulation, journal, suppression d'image.
- [workflows/workflows.go](../../internal/modules/builds/workflows/workflows.go), [activities/](../../internal/modules/builds/activities/) — workflow, Job, détection, fin.
- [registry/](../../internal/modules/builds/registry/) — identifiants, jetons, client du registre.
- [images/images.go](../../internal/modules/builds/images/images.go) — taille, ménage, quota.
- [builds_wiring.go](../../internal/infrastructure/router/builds_wiring.go), [route.go](../../internal/infrastructure/router/route.go) — câblage.
