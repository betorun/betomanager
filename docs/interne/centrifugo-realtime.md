# Centrifugo realtime E2E

> Stack realtime : kubemanager signe des JWT HS256 → frontend ouvre WS Centrifugo → kubemanager publie via HTTP API.

## Setup local

- Service `centrifugo` dans `docker-compose.yaml` (port 8000).
- Vars d'env : `CENTRIFUGO_TOKEN_HMAC_SECRET_KEY` (attention suffixe `_KEY`), `CENTRIFUGO_API_KEY`, `CENTRIFUGO_ALLOWED_ORIGINS`.
- Le secret HMAC doit être identique côté kubemanager (`KUBEMANAGER_CENTRIFUGO_TOKEN_HMAC_SECRET`).

## Test E2E

Script : `scripts/test-realtime-e2e.sh` — automatise l'obtention d'un token, l'ouverture d'une WS, la publication serveur, la validation du broadcast.

## Channels

- `project:<id>:events`
- `project:<id>:workload:<id>`
- `project:<id>:workload:<id>:deploy`

> **Stub** — ajouter la matrice de troubleshooting (secret mismatch, CORS, API key).
