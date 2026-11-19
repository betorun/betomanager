# Module `registries`

> Accès d'un projet à ses registres d'images externes (Docker Hub, ghcr.io, GitLab…) : le jeton est chiffré, ne se relit jamais et ne sert qu'à tirer les images privées du projet.

Décisions : [ADR 0011](../adr/0011-registres-externes.md). Le registre de la plateforme (images construites par betorun) relève du module [`builds`](builds.md).

## Modèle

Table `project_registry_credentials` (migration 000023) :

| Colonne | Rôle |
|---|---|
| `id`, `project_id` | un accès appartient à un projet (supprimé avec lui), jamais partagé |
| `host` | hôte normalisé : `ghcr.io`, `docker.io`, `registry.gitlab.com`, port facultatif |
| `username` | en clair (affiché) |
| `sealed_password` | jeton chiffré (AES-GCM, `auth.Sealer`), données associées `registry-credential\|<id>\|<host>` |
| `created_by`, `created_at`, `updated_at` | suivi |

Unicité `(project_id, host)` : un seul accès par registre et par projet. Les données associées lient le chiffré à sa ligne et à son hôte : recopié ailleurs, il ne s'ouvre plus.

**Hôte** ([registries.go](../../internal/modules/registries/domain/registries.go)) : minuscules, sans schéma, chemin ni identifiants, avec un point ou un port. Les alias de Docker Hub (`index.docker.io`, `registry-1.docker.io`, `registry.hub.docker.com`, `hub.docker.com`) deviennent `docker.io` ; dans le Secret de pull, Docker Hub est écrit sous la clé `https://index.docker.io/v1/` attendue par containerd. L'hôte d'une image suit les règles de Docker : `nginx` ou `moi/app` → `docker.io`. Le registre de la plateforme est refusé (`REGISTRY_IS_PLATFORM`).

**Identifiants** : utilisateur non vide (255 caractères, sans `:` ni saut de ligne), jeton non vide (4096 caractères, sans saut de ligne).

## Endpoints HTTP

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/projects/:id/registries` | `workloads.registries.list` | Accès du projet : hôte, utilisateur, dates (jamais le jeton) |
| PUT | `/projects/:id/registries/:host` | `workloads.registries.manage` | Crée ou remplace l'accès (`username`, `password`) |
| DELETE | `/projects/:id/registries/:host` | `workloads.registries.manage` | Retire l'accès — 204 |
| POST | `/projects/:id/registries/:host/test` | `workloads.registries.manage` | Vérifie l'accès : `{"result": "ok" \| "denied" \| "unreachable"}` |

Rôles ([roles.go](../../internal/iam/roles.go)) : `list` dès le rôle développeur (et `roles/workloads.developer`) ; `manage` à partir d'éditeur seulement, car un jeton de registre ouvre les images privées de l'entreprise. Le lecteur ne voit pas les registres.

Erreurs ([errors.go](../../internal/infrastructure/router/errors.go)) : `REGISTRY_INVALID_HOST` (400), `REGISTRY_IS_PLATFORM` (400), `INVALID_INPUT` (400, identifiants), `REGISTRY_NOT_FOUND` (404, aucun accès pour cet hôte).

CLI ([registries.go](../../internal/cli/registries.go)) : `betorun registries list | add <hôte> | test <hôte> | remove <hôte>`.

## Déploiement d'une image privée

Un seul Secret `kubernetes.io/dockerconfigjson` par namespace de projet, `betorun-registry` (nom historique, cité par les déploiements existants), assemblé par [pull.go](../../internal/modules/registries/pull/pull.go) :

- l'identifiant `pull.<projet>` du registre de la plateforme (si les builds sont activés) ;
- les accès externes du projet, déchiffrés à ce moment-là.

1. À la préparation d'un déploiement, le service des conteneurs note `ExternalPull` si l'image ne vient pas du registre de la plateforme et que le projet a un accès pour son hôte ([domains.go](../../internal/modules/workloads/service/domains.go)).
2. Le workflow appelle l'activité `EnsureRegistryPullSecret` (image de la plateforme ou accès externe) : le Secret est écrit côté worker, les valeurs ne passent jamais par l'historique Temporal ([heml.go](../../internal/modules/workloads/activities/heml.go)). Le chart reçoit `pullSecrets: [betorun-registry]`.
3. Si l'accès a été retiré entre-temps et qu'il ne reste aucune entrée, le Secret est supprimé et le déploiement échoue (`NO_REGISTRY_ACCESS`).

Après un `PUT` ou un `DELETE`, l'API réécrit tout de suite le Secret s'il existe déjà (namespace déjà déployé), même si le client a raccroché (délai de 15 s). Un échec de cette réécriture n'annule pas la modification : il est journalisé et le prochain déploiement réécrit le Secret.

## Test d'un accès

[prober.go](../../internal/modules/registries/service/prober.go) : `GET https://<hôte>/v2/`, puis suit le défi `WWW-Authenticate` (`Basic` ou `Bearer` avec son `realm`, en HTTPS seulement) avec les identifiants. `ok` si accepté, `denied` si refusé, `unreachable` sinon. Garde-fous anti-SSRF : HTTPS seulement, adresses privées, locales, de bouclage et CGNAT (100.64/10) refusées au moment de la connexion (une résolution DNS ne peut pas le contourner), pas de proxy, délai de 10 s, trois redirections HTTPS au plus, réponse lue sur 64 Kio au plus.

## Configuration

Pas de section propre : le module dépend de la clé de chiffrement de `auth.Sealer` (partagée avec les variables secrètes) et lit `builds.registry_host` pour refuser le registre de la plateforme. Sans client Kubernetes, aucun Secret n'est réécrit côté API.

## Pièges

- **Jeton jamais relisible** : on le remplace par un `PUT`. Il n'apparaît ni dans les réponses, ni dans les journaux, ni dans Temporal. Conseiller un jeton en lecture seule (GitHub `read:packages`, Docker Hub « Read-only », GitLab `read_registry`) : betorun ne peut pas le vérifier.
- **Clé de chiffrement** : la changer rend illisibles les accès enregistrés (déploiements et tests en erreur).
- **Un registre « ouvert »** (`/v2/` répond 200 sans authentification) : le test vérifie seulement que les identifiants ne sont pas refusés.
- **Hors V1** : ECR et Artifact Registry (jetons à renouveler), plusieurs accès pour un même hôte, accès partagés entre projets.
- L'ADR mentionne les noms `betorun-pull` et `project:registries:*` : le code utilise `betorun-registry` et `workloads.registries.*`.

## Fichiers clés

- [registries.go](../../internal/modules/registries/registries.go) — handlers.
- [domain/registries.go](../../internal/modules/registries/domain/registries.go) — modèle, normalisation des hôtes, erreurs.
- [service/service.go](../../internal/modules/registries/service/service.go), [service/prober.go](../../internal/modules/registries/service/prober.go) — logique et test d'accès.
- [repository/repository.go](../../internal/modules/registries/repository/repository.go) — stockage chiffré.
- [pull/pull.go](../../internal/modules/registries/pull/pull.go) — Secret de pull du projet.
- [route.go](../../internal/infrastructure/router/route.go) (`addRegistriesRoutes`), migration [000023](../../migrations/000023_registry_credentials.up.sql).
