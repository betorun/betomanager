# ADR 0011 — Accès aux registres d'images externes

Statut : implémenté (module `registries`). Date : 2026-11-18.

## Problème

Un client peut déployer une image publique (Docker Hub, ghcr.io, quay.io…) ou une
image construite par betorun. Il ne peut pas déployer une image **privée** d'un autre
registre (Docker Hub privé, GitHub Container Registry, GitLab, ECR…) : le cluster
n'a aucun identifiant pour la tirer et le pod reste en `ImagePullBackOff`, sans
message compréhensible.

C'est le cas le plus courant chez un développeur qui a déjà une CI : elle pousse une
image privée, il veut juste qu'on la déploie.

## Ce qui existe déjà et qu'on réutilise

- Chiffrement AES-GCM avec données associées (`auth.Sealer`), utilisé par les
  variables (`workload_variables`, AAD `workload-variable|<id>|<nom>`).
- Secret de pull créé côté worker, hors de l'historique Temporal :
  `EnsureRegistryPullSecret` (activities/heml.go), une seule entrée, notre registre,
  et seulement si `input.PlatformImage`.
- Le chart accepte une liste `image.pullSecrets`.
- Contrôle `checkImage` : une image de notre registre doit appartenir au projet.

## Décision proposée

### Modèle

Nouvelle table `project_registry_credentials` (migration 000023) :

| Colonne | Rôle |
|---|---|
| `id` uuid | identifiant |
| `project_id` uuid, FK | un accès appartient à un projet, jamais partagé |
| `host` text | `ghcr.io`, `docker.io`, `registry.gitlab.com`, `<id>.dkr.ecr.<région>.amazonaws.com` |
| `username` text | en clair (non secret, affiché) |
| `sealed_password` bytea | jeton chiffré, AAD `registry-credential|<id>|<host>` |
| `created_by`, `created_at`, `last_used_at`, `last_error` | suivi et diagnostic |

Unicité `(project_id, host)` : un seul accès par registre et par projet en V1.
Le registre de la plateforme est refusé comme `host` (il a déjà son accès).

### API

```
GET    /projects/:id/registries           liste (jamais le jeton)
PUT    /projects/:id/registries/:host     créer ou remplacer (username, password)
DELETE /projects/:id/registries/:host
POST   /projects/:id/registries/:host/test   vérifie l'accès (voir plus bas)
```

Permissions IAM nouvelles : `workloads.registries.list`, `workloads.registries.manage`
(rôles propriétaire et éditeur ; pas développeur par défaut, car un jeton de
registre donne accès au code de l'entreprise). *Écrit d'abord
`project:registries:view/manage`, « propriétaire et administrateur » : le code fait
foi, l'éditeur est le rôle le plus proche d'un administrateur.*

### CLI

```
betorun registries list
betorun registries add ghcr.io --user moi     # jeton lu au clavier ou sur stdin, jamais en argument
betorun registries test ghcr.io
betorun registries remove ghcr.io
```

### Console

Page « Registres » à part dans la console (pas dans les réglages du projet) : liste,
ajout, test, suppression.
Aide par fournisseur (où créer un jeton en lecture seule : GitHub « read:packages »,
Docker Hub « Read-only access token », GitLab « read_registry »).

### Déploiement

1. `deployInput` indique si l'image vient d'un registre pour lequel le projet a un
   accès (comparaison du `host` normalisé ; `nginx` → `docker.io`).
2. Le worker construit **un seul** Secret `kubernetes.io/dockerconfigjson` par
   namespace, `betorun-registry` (écrit d'abord `betorun-pull`), avec notre registre
   et les accès externes du projet.
   Les valeurs sont déchiffrées dans l'activité, jamais dans l'entrée du workflow.
3. Le chart reçoit `pullSecrets: [betorun-registry]` dès qu'une entrée s'applique.
4. Suppression ou modification d'un accès : le Secret est réécrit au prochain
   déploiement ; une suppression le réécrit tout de suite (pas d'accès résiduel).

### Erreurs compréhensibles

Le watcher remonte déjà les événements de pods. On traduit `ErrImagePull` et
`ImagePullBackOff` en statut clair : « le registre ghcr.io refuse l'accès : vérifiez
le jeton » ou « image introuvable : vérifiez le nom et l'étiquette ». Le message brut
du pod n'est jamais affiché tel quel (il peut contenir des informations sensibles).

Implémenté (2026-11-19) dans l'état en direct du conteneur : `pull_failure`
`{cause, registry, image}`, cause parmi `denied`, `not_found`, `denied_or_not_found`
(Docker Hub ne distingue pas un dépôt privé d'un dépôt absent), `unreachable`,
`invalid`, `unknown`. Le classement lit l'état du conteneur, puis le dernier
événement `Failed` du pod quand ImagePullBackOff ne donne que « Back-off pulling
image » ([domain/pull.go](../../internal/modules/workloads/domain/pull.go)). La
console et `betorun workloads status` en font la phrase.

### Test d'un accès

`POST …/test` appelle `GET https://<host>/v2/` puis suit le défi d'authentification
(`WWW-Authenticate`) avec les identifiants, depuis l'API. Résultat : valide,
refusé, ou registre injoignable. Garde-fous : HTTPS obligatoire, adresses privées et
locales refusées (même liste que l'egress des builds), délai court, aucune
redirection vers une adresse privée. Sans cela, l'API deviendrait un outil de
requêtes vers le réseau interne (SSRF).

## Sécurité

- Jeton jamais relisible : on le remplace, on ne le consulte pas.
- Jamais dans Temporal, jamais dans les journaux ni dans l'audit (seulement l'hôte
  et l'utilisateur).
- Le Secret Kubernetes vit dans le namespace du projet : un autre client ne le voit
  pas. Le client lui-même n'a pas accès à l'API Kubernetes.
- Recommandation affichée partout : jeton en **lecture seule**. On ne peut pas le
  vérifier côté betorun, on le conseille.
- ECR : ses jetons expirent après 12 heures. Hors V1 (voir plus bas).

## Hors V1

- ECR et GCP Artifact Registry, qui demandent des jetons renouvelés automatiquement.
- Plusieurs accès pour un même hôte (organisations différentes sur ghcr.io).
- Accès partagés entre projets d'une même organisation.
- `betorun registry login` pour pousser dans notre registre : chantier séparé, qui
  demande d'abord un quota de stockage d'images et un nettoyage des anciennes
  versions.

## Effort estimé

Moyen : migration et dépôt, service et API, activité de pull, CLI, page console,
traduction des erreurs de pull, tests (unitaires, vraie base, et un test réel sur un
paquet privé ghcr.io).

## Décisions (validées le 2026-11-18)

1. Gestion des accès : propriétaire et éditeur seulement, pas les développeurs.
2. ECR : hors V1.
3. Un accès par hôte et par projet en V1.

## Emplacement du code

- Nouveau module `internal/modules/registries/` (domain, repository, service,
  handlers), comme les autres modules : il gère les accès des clients.
- `internal/modules/builds/registry/` reste le registre de la plateforme (jetons,
  identifiants de build et de pull) ; il n'est pas déplacé.
- `workloads/activities/heml.go` : `EnsureRegistryPullSecret` devient
  `EnsurePullSecret` et assemble notre registre et les accès externes du projet.
- Migration 000023, CLI `internal/cli/registries.go`, page console « Registres ».
