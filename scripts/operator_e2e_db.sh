#!/usr/bin/env bash
# Postgres jetable pour les tests de la base de l'Opérateur
# (internal/operator/store) ; les migrations sont appliquées par le test.
#
#   bash scripts/operator_e2e_db.sh
#   OPERATOR_E2E_DSN='postgres://postgres:pw@127.0.0.1:55434/postgres?sslmode=disable' \
#     go test ./internal/operator/store/ -count=1
set -euo pipefail
docker rm -f operator-e2e-pg >/dev/null 2>&1 || true
docker run -d --rm --name operator-e2e-pg -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55434:5432 postgres:15-alpine >/dev/null
until docker exec operator-e2e-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
sleep 1
