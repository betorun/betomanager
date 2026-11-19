# Module `iamapi`

> API HTTP de l'IAM : policies d'autorisation, test de permissions, catalogues, rôles personnalisés, groupes, comptes de service et leurs clés, fédération OIDC, STS, journal d'audit, politique de sécurité d'organisation et clés des watchers. La conception (modèle de ressources, principals, rôles, STS, clés) est dans [docs/architecture/iam.md](../architecture/iam.md) ; cette page ne décrit que les routes.

## Architecture

`iamapi.Controller` est une couche mince : il lit la requête, retrouve l'appelant (`security.PrincipalFromContext`) et délègue aux services de `internal/iam` (`PolicyService`, `Engine`, `CustomRoleService`, `GroupService`, `ServiceAccountService`, `OrgPolicyService`, `ClusterAgents`) et à `internal/iam/sts`. Le contrôle d'accès est déclaré sur chaque route (`g.Perm` / `g.PermFor`) dans [route.go](../../internal/infrastructure/router/route.go) (`addIAMRoutes`). Les permissions marquées `Sensitive` dans [permissions.go](../../internal/iam/permissions.go) exigent une double authentification récente (`iam.step_up_max_age`) et sont refusées à un jeton délégué (Copilot).

Les ressources sont désignées par leur nom IAM (`organizations/<uuid>`, `projects/<uuid>`, sous-ressources d'un projet).

## Endpoints HTTP

Préfixe `/kmanager/v1`.

### Policies, permissions, rôles (`/iam`)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/iam/policy?resource=` | `resourcemanager.organizations.getIamPolicy` ou `resourcemanager.projects.getIamPolicy` | Policy (bindings, etag, membres décrits) ; vide avec etag `ACAB` si aucune |
| POST | `/iam/policy` | `resourcemanager.organizations.setIamPolicy` ou `resourcemanager.projects.setIamPolicy` (sensibles) | Remplace la policy ; etag obligatoire |
| POST | `/iam/testPermissions` | `Authenticated()` | Sous-ensemble des permissions demandées accordé à l'appelant (100 au plus, noms connus) |
| GET | `/iam/roles` | `Authenticated()` | Rôles prédéfinis, titres traduits |
| GET | `/iam/permissions` | `Authenticated()` | Catalogue des permissions (hors `PlatformOnly`), avec `custom_roles` |
| GET | `/iam/session` | `Authenticated()` | État d'authentification : MFA disponible, session MFA, validité de la confirmation récente |
| GET | `/iam/ancestry?resource=` | `resourcemanager.projects.get` ou `resourcemanager.organizations.get` | Ascendance (projet → organisation → platform) |
| GET | `/iam/auditLogs?resource=` | `logging.auditLogs.read` | Journal d'une ressource et de ses descendantes ; `limit` 1 à 200 (défaut 50), curseur `before_id` → `next_before_id` (0 = fin). `organizations/<id>` : actions sur l'organisation seule (accès, groupes, rôles, projets créés), pas celles des projets. `projects/<id>` : en plus, les actions d'organisation qui touchent le projet (accès de l'organisation, groupes et rôles d'organisation liés au projet, d'après les bindings actuels) |

Pour une sous-ressource de projet, la permission de policy est celle du projet ; la policy `platform` ne passe pas par l'API.

### Rôles personnalisés (`/iam/customRoles`, `/iam/customRole`)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/iam/customRoles?parent=` | `iam.roles.list` | Rôles d'une organisation ou d'un projet |
| POST | `/iam/customRoles` | `iam.roles.create` (sensible) | Créer, avec des permissions détenues par l'appelant |
| GET | `/iam/customRole?name=` | `iam.roles.list` | Détails |
| PATCH | `/iam/customRole` | `iam.roles.update` (sensible) | Modifier (etag) ; effet immédiat pour tous les détenteurs |
| DELETE | `/iam/customRole?name=` | `iam.roles.delete` (sensible) | Supprimer, avec tous ses bindings |

Nom : `organizations|projects/<uuid>/roles/<id>`. Seules les permissions marquées `CustomRoles` sont admises.

### STS (publics, la preuve est dans la requête)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| POST | `/iam/token` | `Public()` | `jwt-bearer` (RFC 7523, assertion signée par une clé de compte de service, 1 h au plus) ou `token-exchange` (RFC 8693, jeton OIDC fédéré ou jeton Kubernetes d'un workload, 15 min) ; erreurs au format RFC 6749 §5.2 |
| GET | `/iam/.well-known/jwks.json` | `Public()` | Clés publiques de signature (`max-age=300`) |
| GET | `/iam/.well-known/openid-configuration` | `Public()` | Métadonnées de l'émetteur (`max-age=3600`) |

### Organisation : sécurité et groupes (`/organizations/:id`)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/securityPolicy` | `resourcemanager.organizations.get` | Politique de sécurité |
| PUT | `/securityPolicy` | `resourcemanager.organizations.update` (sensible) | `require_mfa` pour l'organisation et ses projets |
| GET / POST | `/groups` | `iam.groups.list` / `iam.groups.create` | Lister / créer |
| GET | `/groups/:groupID` | `iam.groups.get` | Détails |
| PATCH / DELETE | `/groups/:groupID` | `iam.groups.update` / `iam.groups.delete` (sensibles) | Modifier / supprimer (ses rôles partout compris) |
| GET | `/groups/:groupID/members` | `iam.groups.get` | Membres |
| POST / DELETE | `/groups/:groupID/members` | `iam.groups.update` (sensible) | Ajouter / retirer (`?member=`) |

### Comptes de service et clés (`/projects/:id/serviceAccounts`)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET / POST | (racine) | `iam.serviceAccounts.list` / `iam.serviceAccounts.create` | Lister / créer (sans rôle ; `account_id` 6-30 caractères) |
| GET / DELETE | `/:saID` | `iam.serviceAccounts.get` / `iam.serviceAccounts.delete` (sensible) | Détails / supprimer (clés et rôles compris) |
| GET / POST | `/:saID/keys` | `iam.serviceAccountKeys.list` / `iam.serviceAccountKeys.create` (sensible) | Clés asymétriques ; générée (ES256, clé privée remise une fois) ou clé publique importée |
| DELETE | `/:saID/keys/:keyID` | `iam.serviceAccountKeys.delete` | Désactiver ; jetons émis refusés sous 30 s |
| GET / POST | `/:saID/federation` | `iam.serviceAccountKeys.list` / `iam.serviceAccountKeys.create` (sensible) | Relations de confiance OIDC (GitHub Actions, GitLab CI…) |
| DELETE | `/:saID/federation/:trustID` | `iam.serviceAccountKeys.delete` | Supprimer ; jetons émis refusés sous 30 s |
| GET / POST | `/:saID/apiKeys` | `iam.apiKeys.list` / `iam.apiKeys.create` (sensible) | Clés API ; secret affiché une seule fois ; `env` `live`/`test`, 1 à 90 jours, restrictions facultatives |
| DELETE | `/:saID/apiKeys/:keyID` | `iam.apiKeys.delete` | Révoquer (effet immédiat) |

Créer une clé, une clé asymétrique ou une relation de confiance exige en plus `iam.serviceAccounts.actAs` sur le compte (vérifié par le service).

### Staff et watchers (internes)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| POST | `/admin/cluster-keys` | `platform.clusterKeys.create` (sensible) | Clé de watcher (crée le compte système du cluster au besoin) |
| GET | `/admin/cluster-keys` | `platform.clusterKeys.list` | Clés v2 et legacy, avec leur état |
| POST | `/admin/cluster-keys/:keyID/rotate` | `platform.clusterKeys.rotate` (sensible) | Rotation sans coupure (période de grâce) |
| DELETE | `/admin/cluster-keys/:keyID` | `platform.clusterKeys.revoke` (sensible) | Révocation immédiate |
| POST | `/clusters/ClusterService/:clusterID/ServiceAccountKeys` | garde watcher | Clés publiques des jetons Kubernetes du cluster (identité des workloads) |

## Règles appliquées par les services

- **Policy** (`PolicyService.Set`) : read-modify-write par etag (`ErrEtagRequired`, `ErrEtagMismatch`) ; membres `user:<uuid>` ou `user:<email>` existants ; anti-escalade : on n'accorde un rôle que si l'on en détient toutes les permissions sur la ressource ; une organisation ou un projet garde au moins un owner humain.
- **Rôles personnalisés** : créer ou étendre un rôle exige de détenir ces permissions sur le parent ; supprimer un rôle retire ses bindings (un nom réutilisé ne ressuscite aucun droit).
- **Groupes** : pas d'imbrication ; ajouter un membre exige de détenir toutes les permissions des rôles du groupe sur chaque ressource ; retirer est libre.
- **testPermissions** n'expose que les droits de l'appelant lui-même.

## Configuration

Clés `iam.*` ([config.go](../../internal/core/configs/config.go)) : `api_key_pepper` (secret, ≥ 32 octets ; le changer invalide toutes les clés API), `sts_issuer` (`<API>/kmanager/v1/iam`), `sts_key_encryption_key` (secret, 32 octets en base64), `mfa_acr_values` (vide = pas de step-up ni d'exigence MFA possible), `step_up_max_age` (10 min par défaut), `platform_admins` / `platform_support` (emails du staff).

## Pièges

- `POST /iam/policy` remplace la policy entière : relire d'abord (`GET`) et renvoyer l'etag.
- Les routes `Authenticated()` (dont `testPermissions`) refusent les jetons délégués du Copilot.
- `/iam/token` ne répond pas au format d'erreur habituel de l'API mais en `{error, error_description}` (OAuth).

## Fichiers clés

- [hiam.go](../../internal/modules/iamapi/hiam.go) — policies, testPermissions, catalogues, comptes de service, clés API, audit.
- [teams.go](../../internal/modules/iamapi/teams.go) — ascendance, rôles personnalisés, groupes.
- [sts.go](../../internal/modules/iamapi/sts.go) — STS, JWKS, découverte, clés asymétriques, fédération.
- [security.go](../../internal/modules/iamapi/security.go) — `/iam/session`, politique de sécurité d'organisation.
- [clusterkeys.go](../../internal/modules/iamapi/clusterkeys.go) — clés des watchers, clés Kubernetes rapportées.
- [policy.go](../../internal/iam/policy.go), [customroles.go](../../internal/iam/customroles.go), [groups.go](../../internal/iam/groups.go), [serviceaccounts.go](../../internal/iam/serviceaccounts.go), [sakeys.go](../../internal/iam/sakeys.go), [federation.go](../../internal/iam/federation.go) — règles métier.
- [permissions.go](../../internal/iam/permissions.go) — catalogue et drapeaux `Sensitive` / `CustomRoles` / `PlatformOnly`.
