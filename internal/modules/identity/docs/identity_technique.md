# Module `identity`

> Bootstrap Keycloak : register, login (ROPC), refresh, social OAuth (Google/GitHub).

## Responsabilités
- Création d'un user Keycloak + sync local DB.
- Login ROPC (password grant) → JWT kubemanager.
- Refresh token rotation (stocké en DB, audit `last_used_at`).
- Social OAuth : callback `/auth/callback/:provider` → upsert user.
- Admin REST API Keycloak : groupes, rôles, reset password.

## Fichiers
- `internal/modules/identity/` — service + controller
- `internal/infrastructure/keycloak/` — client Keycloak (admin REST + token endpoint)

> **Stub** — à enrichir (realm config, scripts bootstrap, mapping claims).
