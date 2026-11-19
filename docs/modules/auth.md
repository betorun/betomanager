# Module `auth`

> Users, sessions, API keys, RBAC projet. C'est le module fondation — tous les autres modules l'utilisent pour savoir qui fait quoi.

## Responsabilités

- Persistance des users (miroir local de Keycloak via `keycloak_user_id`).
- Sessions utilisateurs (JWT access + refresh token).
- Révocation de tokens (table `revoked_tokens`).
- API keys duales : kind `user` (CLI devs) et kind `cluster` (watchers K8s).
- RBAC projet : `ProjectMember` + rôles + permissions.
- Accumulation batch du `usage_count` API keys (éviter hot row).

## Endpoints HTTP

Tous sous `/kmanager/v1/users/api-keys`.

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `GET` | `/` | Liste les clés du user courant |
| `POST` | `/` | Crée une nouvelle clé (token clair retourné UNE fois) |
| `GET` | `/audit` | Vue audit (usage_count, last_used_at, status) |
| `GET` | `/:id` | Introspection d'une clé (sans hash) |
| `DELETE` | `/:id` | Révocation |

Endpoints admin (group `/admin/cluster-keys`, guard `RequireGlobalRole(SUPER_ADMIN, ADMIN)`) :

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/admin/cluster-keys` | Provisionne une clé `kind=cluster` pour un watcher |
| `GET` | `/admin/cluster-keys` | Liste toutes les cluster keys |
| `POST` | `/admin/cluster-keys/:id/rotate` | Rotation avec grace period |

Déclarations : [internal/infrastructure/router/route.go:98-126](../../internal/infrastructure/router/route.go#L98).

## Tables DB

| Table | Rôle |
|-------|------|
| `users` | profils (miroir Keycloak) |
| `user_sessions` | refresh tokens actifs (hashés) |
| `revoked_tokens` | liste de JTI révoqués jusqu'à expiration |
| `api_keys` | clés + hash BCrypt + scopes |
| `roles`, `permissions`, `role_permissions` | RBAC canonique |
| `project_members` | user × project × role |

Schéma : [migrations/000001_init.up.sql](../../migrations/000001_init.up.sql).

## Activities Temporal

Aucune. Le module est purement synchrone — les effets externes (Keycloak) sont dans `identity`.

## Invariants critiques

1. **`userID` toujours extrait du JWT côté serveur** — jamais du payload client. Le handler lit `c.GetString("userID")` (posé par AuthMiddleware).
2. **Le token clair d'une API key n'est retourné qu'à la création**. Jamais relu.
3. **Le hash est BCrypt**, la comparaison utilise `subtle.ConstantTimeCompare`.
4. **Scopes vérifiés côté middleware** via `RequireAPIKeyKind` / `RequireAPIKeyScope` avant d'atteindre le handler.
5. **Le `usage_count` est agrégé en mémoire** (via `UsageAccumulator`) et flushé en batch via `BatchIncrementAPIKeyUsage` — NE JAMAIS faire `UPDATE api_keys SET usage_count = usage_count + 1` dans un hot path.

## Pitfalls

- **Timing attack** — si tu changes la comparaison du hash, garde `subtle.ConstantTimeCompare`. Un `bytes.Equal` leakerait des bits d'info.
- **Fuite de la clé en log** — bannir complètement. Seul le `prefix` peut être loggé.
- **Membership stale** — après `RemoveProjectMember`, le JWT du user reste valide jusqu'à expiration. Un check `CheckProjectPermission` doit être fait à chaque requête (pas de cache mémoire côté user).

## Tests

- [service/apikey_test.go](../../internal/modules/auth/service/apikey_test.go) — création, révocation, hash compare, scopes.
- [service/usage_accumulator_test.go](../../internal/modules/auth/service/usage_accumulator_test.go) — batch increment + concurrence.
- [middleware/security/middleware_test.go](../../internal/middleware/security/middleware_test.go) — chain auth (JWT + API key), edge cases (expired, revoked, inactive).

## Fichiers clés

- [domain/entities.go](../../internal/modules/auth/domain/entities.go) — GORM entities.
- [domain/permissions.go](../../internal/modules/auth/domain/permissions.go) — PermissionType enum.
- [repository/gorm_repository.go](../../internal/modules/auth/repository/gorm_repository.go) — AuthRepository (30+ méthodes).
- [service/apikey.go](../../internal/modules/auth/service/apikey.go) — APIKeyService.
- [hApikey.go](../../internal/modules/auth/hApikey.go) — controller.

## Points d'attention ajout de feature

- Nouvelle permission → ajouter dans [permissions.go](../../internal/modules/auth/domain/permissions.go) + migration pour seed dans `roles` si la permission doit être attachée à un rôle par défaut.
- Nouveau scope API key → ajouter dans la doc ([glossaire](../onboarding/04-glossaire.md)) + dans le `CreateAPIKey` validator.
- Nouveau kind API key → ajouter le middleware `RequireAPIKeyKind` sur les routes concernées.
