# Module `auth`

> Users, clés API legacy, membres de projet (modèle historique). Les droits eux-mêmes sont passés à l'IAM ([internal/iam](../../internal/iam/), [iam.md](../architecture/iam.md)) ; les sessions et la connexion des personnes sont dans `identity` + [internal/auth](../../internal/auth/) ([auth.md](../architecture/auth.md)).

## Responsabilités

- Persistance des users (miroir local de Keycloak via `keycloak_user_id`).
- Clés API utilisateur **legacy** (`km_…`) : listées, auditées, révoquées. Elles ne sont **plus émises** (`ErrUserAPIKeysRetired`) : les nouvelles clés sont celles des comptes de service (`POST /projects/:id/serviceAccounts/:saID/apiKeys`, voir [iam.md §8](../architecture/iam.md)). Migration 000007 : chaque clé restante est rattachée à un compte de service `legacy-<hash>` par projet et expire sous 90 jours.
- Membres de projet (`/rbac`) : façade historique au-dessus des bindings IAM ([repository/members_iam.go](../../internal/modules/auth/repository/members_iam.go)) ; `project_members` n'existe plus (migration 000006).
- Validation des clés API dans le middleware (préfixe, hash, révocation, cache 30 s).
- Accumulation batch du `usage_count` API keys (éviter hot row).

Les clés des watchers (`/admin/cluster-keys`) sont gérées par le module `iamapi` (voir plus bas).

## Endpoints HTTP

Sous `/kmanager/v1/users/api-keys` — accès `Authenticated()` : le service filtre par l'utilisateur appelant.

| Méthode | Chemin | Accès | Description |
|---------|--------|-------|-------------|
| `GET` | `/` | authentifié | Liste les clés du user courant |
| `POST` | `/` | authentifié | **Refusé** : clés utilisateur retirées (message vers les clés de compte de service) |
| `GET` | `/audit` | authentifié | Vue audit (usage_count, last_used_at, status `active`/`revoked`/`expired`) |
| `GET` | `/:id` | authentifié | Introspection d'une clé (sans hash) |
| `DELETE` | `/:id` | authentifié | Révocation |

Sous `/kmanager/v1/rbac` (membres de projet, modèle historique) :

| Méthode | Chemin | Accès | Description |
|---------|--------|-------|-------------|
| `GET` | `/roles` | authentifié | Rôles legacy (OWNER, DEVELOPER, VIEWER) ; les rôles IAM sont sur `GET /iam/roles` |
| `POST` | `/assign-role` | `resourcemanager.projects.setIamPolicy` (projet du corps, `project_id`) | Assigner un rôle |
| `GET` | `/:projectID/members` | `resourcemanager.projects.getIamPolicy` | Membres du projet |
| `DELETE` | `/:projectID/members/:userID` | `resourcemanager.projects.setIamPolicy` | Révoquer l'accès d'un membre |

Clés des watchers — `/kmanager/v1/admin/cluster-keys`, route interne, contrôleur `iamapi` (staff, permissions sur `platform`) :

| Méthode | Chemin | Permission | Description |
|---------|--------|-----------|-------------|
| `POST` | `/admin/cluster-keys` | `platform.clusterKeys.create` | Crée une clé de watcher (et le compte de service système du cluster au besoin) ; secret affiché une fois |
| `GET` | `/admin/cluster-keys` | `platform.clusterKeys.list` | Liste les clés (v2 et legacy : active, expiring, expired, revoked) |
| `POST` | `/admin/cluster-keys/:keyID/rotate` | `platform.clusterKeys.rotate` | Rotation avec période de grâce |
| `DELETE` | `/admin/cluster-keys/:keyID` | `platform.clusterKeys.revoke` | Révocation immédiate |

Déclarations : `addAPIKeyRoutes`, `addRBACRoutes` et `addIAMRoutes` dans [internal/infrastructure/router/route.go](../../internal/infrastructure/router/route.go).

## Tables DB

| Table | Rôle |
|-------|------|
| `users` | profils (miroir Keycloak) |
| `user_sessions`, `revoked_tokens` | anciennes sessions et JTI révoqués (sessions Keycloak d'avant la migration) ; les sessions actuelles sont dans `auth_sessions` (module `identity`) |
| `api_keys` | clés legacy : préfixe + hash SHA-256 + scopes, `service_account_id` (000007) |
| `iam_bindings`, `iam_policies` | membres de projet (rôles) — tables de l'IAM |

`roles`, `permissions`, `role_permissions` (000007) et `project_members` (000006) ont été supprimées : les rôles vivent dans le code ([internal/iam](../../internal/iam/)).

Schéma initial : [migrations/000001_init.up.sql](../../migrations/000001_init.up.sql).

## Activities Temporal

Aucune. Le module est purement synchrone — les effets externes (Keycloak) sont dans `identity`.

## Invariants critiques

1. **`userID` toujours extrait du contexte posé par le middleware** — jamais du payload client.
2. **Le token clair d'une API key n'était retourné qu'à la création**. Jamais relu.
3. **Le hash est SHA-256** (`utils.HashAPIKey`), la comparaison utilise `subtle.ConstantTimeCompare` ([middleware/security/middleware.go](../../internal/middleware/security/middleware.go), `handleAPIKey`).
4. **Les droits d'une clé sont ceux de son compte de service** : une clé rattachée à un compte de service agit comme `serviceAccount:<id>` (pas de `userID` dans le contexte) et chaque route vérifie sa permission IAM (`g.Perm(...)`). Les scopes ne sont plus le contrôle d'accès.
5. **Le `usage_count` est agrégé en mémoire** (via `UsageAccumulator`) et flushé en batch via `BatchIncrementAPIKeyUsage` — NE JAMAIS faire `UPDATE api_keys SET usage_count = usage_count + 1` dans un hot path.

## Pitfalls

- **Timing attack** — si tu changes la comparaison du hash, garde `subtle.ConstantTimeCompare`. Un `bytes.Equal` leakerait des bits d'info.
- **Fuite de la clé en log** — bannir complètement. Seul le `prefix` peut être loggé.
- **Révocation** — le préfixe révoqué est mis en cache (30 j) pour rejeter la clé avant l'expiration du cache de validation (30 s).

## Tests

- [service/apikey_test.go](../../internal/modules/auth/service/apikey_test.go) — révocation (propagation au cache, autre utilisateur, clé inconnue, identifiants invalides).
- [service/usage_accumulator_test.go](../../internal/modules/auth/service/usage_accumulator_test.go) — batch increment + concurrence.
- [middleware/security/middleware_test.go](../../internal/middleware/security/middleware_test.go) — chain auth (JWT + API key), edge cases (expired, revoked, inactive).

## Fichiers clés

- [domain/entities.go](../../internal/modules/auth/domain/entities.go) — GORM entities.
- [domain/permissions.go](../../internal/modules/auth/domain/permissions.go) — rôles et permissions legacy (correspondance des slugs `project:view`…), plus utilisés pour contrôler l'accès aux routes.
- [repository/gorm_repository.go](../../internal/modules/auth/repository/gorm_repository.go) — AuthRepository.
- [repository/members_iam.go](../../internal/modules/auth/repository/members_iam.go) — membres de projet lus et écrits comme bindings IAM.
- [service/apikey.go](../../internal/modules/auth/service/apikey.go) — APIKeyService.
- [service/rbac.go](../../internal/modules/auth/service/rbac.go) — RBACService (membres de projet).
- [hApikey.go](../../internal/modules/auth/hApikey.go), [hRbac.go](../../internal/modules/auth/hRbac.go) — controllers.

## Points d'attention ajout de feature

- Nouvelle permission → l'ajouter dans [internal/iam/permissions.go](../../internal/iam/permissions.go) (constante + entrée du catalogue), l'attacher aux rôles prédéfinis concernés ([internal/iam/roles.go](../../internal/iam/roles.go)), et la passer à `g.Perm(...)` sur la route.
- Nouvelle clé API → passer par les comptes de service (`iamapi`), pas par ce module.
