# Keycloak bootstrap & dev stack

> Setup Keycloak local (docker-compose) + realm `kubemanager` + client + rôles.

## Docker

- Keycloak persistant sur Postgres (service `keycloakdb`).
- MailHog pour capturer les emails (pas de vrai SMTP en dev).
- Realm/client bootstrap via script `scripts/keycloak/bootstrap.sh`.

## Realm

- Realm : `kubemanager`.
- Client : `kubemanager-api` (public, avec ROPC activé en dev uniquement).
- Roles : `SUPER_ADMIN`, `ADMIN`, `USER`.
- Groups : `project-<uuid>` (créés dynamiquement à la création d'un projet).

## Users de test

- `admin@km.local` / `admin` — role `SUPER_ADMIN`.
- `dev@km.local` / `dev` — role `USER`.

> **Stub** — ajouter les scripts exacts + gestion des secrets.
