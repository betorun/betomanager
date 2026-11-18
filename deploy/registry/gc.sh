#!/usr/bin/env bash
# Ménage du registre de la plateforme : libère le disque des images retirées
# par l'API (dernières images gardées par dépôt, voir builds/images).
#
# L'API ne fait que supprimer des manifestes ; les couches restent sur le
# disque tant que ce ménage n'a pas tourné. Pas de --delete-untagged : il
# effacerait aussi les manifestes enfants des index poussés par BuildKit.
#
# Le registre est arrêté pendant le ménage (une à quelques minutes) : un ménage
# pendant un push peut effacer des couches en cours d'envoi. À planifier la nuit,
# par exemple : 30 4 * * 0  /opt/betorun/deploy/registry/gc.sh >> /var/log/betorun-registry-gc.log 2>&1
#
# Usage : gc.sh [--dry-run]
# Variables : COMPOSE_DIR, SERVICE, COMPOSE_PROJECT_NAME (le nom du projet
# Compose désigne les volumes : un mauvais nom pointe vers un registre vide).
set -euo pipefail

COMPOSE_DIR=${COMPOSE_DIR:-$(cd "$(dirname "$0")/../.." && pwd)}
SERVICE=${SERVICE:-registry}
CONFIG=/etc/distribution/config.yml
DRY=()
[[ "${1:-}" == "--dry-run" ]] && DRY=(--dry-run)

cd "$COMPOSE_DIR"
if [[ -z "$(docker compose ps -a -q "$SERVICE")" ]]; then
  echo "service $SERVICE introuvable dans le projet Compose « $(docker compose config --format json | grep -o '"name": *"[^"]*"' | head -1) » : vérifiez COMPOSE_PROJECT_NAME" >&2
  exit 1
fi
echo "$(date -Is) ménage du registre ${DRY[*]:-}"
if [[ ${#DRY[@]} -eq 0 ]]; then
  docker compose stop "$SERVICE"
  trap 'docker compose start "$SERVICE"' EXIT
fi
docker compose run --rm --no-deps --entrypoint registry "$SERVICE" garbage-collect "${DRY[@]}" "$CONFIG" \
  | grep -vE 'eligible for deletion|^[[:space:]]*$' || true
echo "$(date -Is) terminé"
