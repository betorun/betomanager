# `identity` — AI Context

## Invariants
- Keycloak est la source unique pour l'authentification.
- La table `users` miroir Keycloak via `keycloak_user_id` (clé de jointure).
- Les groupes Keycloak reflètent les projets (`project-<uuid>`) pour simplifier l'UI Keycloak admin.

## Règles
- Toujours upsert via `FindByKeycloakUserID` avant de créer un user local.
- Les secrets Keycloak admin sont chargés depuis `config.Keycloak.AdminClientSecret`.
