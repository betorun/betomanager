# Dev stack locale (make dev-up)

> Un seul `make dev-up` pour lancer toute la stack : Postgres, Redis, Keycloak, MailHog, Centrifugo, Temporal, K3s (dans Docker).

## Commandes

| Commande | Effet |
|----------|-------|
| `make dev-up` | Lance docker-compose + bootstrap |
| `make dev-down` | Stop + volumes |
| `make dev-bootstrap` | Seed DB + realm Keycloak + premier user |
| `make dev-logs` | Tail des services critiques |
| `make dev-reset` | Purge complète + relance |

## Services (docker-compose)

- `k8sdb` (Postgres kubemanager) — port 5433 sur host (conflit 5432 local).
- `keycloakdb` (Postgres Keycloak) — non exposé.
- `keycloak` — :8080.
- `mailhog` — :8025 (UI web).
- `centrifugo` — :8000.
- `temporal` — :7233.
- `k3s` (optionnel) — si test du watcher en local.

## Scripts helpers

- `scripts/dev/seed.sh` — seed DB.
- `scripts/keycloak/bootstrap.sh` — realm + users test.
- `scripts/test-realtime-e2e.sh` — test flow Centrifugo.

> **Stub** — ajouter la matrice ports + troubleshooting conflits.
