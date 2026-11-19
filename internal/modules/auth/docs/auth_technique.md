# Module `auth`

> Users, rôles, sessions, API keys, RBAC projet, JWT HS256 + refresh.

## Fichiers
- `internal/modules/auth/domain/` — entities (User, Role, ProjectMember, APIKey, UserSession)
- `internal/modules/auth/repository/` — AuthRepository (GORM)
- `internal/modules/auth/service/` — APIKeyService + services user/session
- `internal/modules/auth/hApikey.go` — IAPIKeyController (list/get/create/revoke + cluster keys)

## Fonctions principales
- Sessions utilisateurs (JWT + refresh token stocké DB).
- Révocation JWT (table `revoked_tokens`).
- RBAC projet : `ProjectMember` avec rôles `owner|admin|developer|viewer` et status `ACTIVE|INVITED|APPROVED`.
- API keys duales (kind `user` / `cluster`).

> **Stub** — section à enrichir. Voir `apikeys` pour les détails sur la gestion des clés.
