#!/bin/bash
# Premier démarrage de Postgres en production (volume vide uniquement) : une
# base et un rôle par service. Un service compromis n'atteint pas les autres.
#
# static-edge ne lit plus Postgres : il tourne dans le cluster et passe par
# l'API (deploy/k8s/static-edge.yaml).
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v keycloak_pw="$KEYCLOAK_DB_PASSWORD" \
  -v temporal_pw="$TEMPORAL_DB_PASSWORD" <<'EOSQL'
CREATE ROLE keycloak LOGIN PASSWORD :'keycloak_pw';
CREATE ROLE temporal LOGIN PASSWORD :'temporal_pw';
CREATE DATABASE keycloak OWNER keycloak;
CREATE DATABASE temporal OWNER temporal;
CREATE DATABASE temporal_visibility OWNER temporal;
EOSQL
