#!/usr/bin/env bash
# Sauvegarde chiffrée de la plateforme vers un stockage objet hors des serveurs
# (deploy/README.txt, section 9). Chaque jour par cron, en root sur le serveur 1 :
#   - tous les jours : bases Postgres, rôles, secrets, sites (SeaweedFS) ;
#   - le dimanche, en plus : registre d'images et certificats de Caddy.
# Le dimanche, tout part sous weekly/ (gardé plus longtemps), sinon sous daily/.
# Code de sortie non nul si une seule pièce manque ; last-success n'avance alors pas.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
secrets="${BETORUN_SECRETS_DIR:-/opt/betorun/secrets}"
compose="${BETORUN_COMPOSE:-$root/deploy/compose.sh}"
# Préfixe des volumes : nom du projet compose (docker-compose.prod.yaml).
project="${BETORUN_PROJECT:-betorun}"
state="${BETORUN_BACKUP_STATE:-/var/lib/betorun-backup}"
env_file="$secrets/backup.env"
image=betorun-backup
tools=alpine:3.23.7

kind="${BACKUP_KIND:-daily}"
[ -z "${BACKUP_KIND:-}" ] && [ "$(date -u +%u)" = 7 ] && kind=weekly
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
prefix="$kind/$stamp"

[ -r "$env_file" ] || { echo "manquant : $env_file" >&2; exit 1; }
docker image inspect "$image" >/dev/null 2>&1 \
  || { echo "image $image absente : docker build -t $image $root/deploy/backup" >&2; exit 1; }
mkdir -p "$state"
chmod 700 "$state"
exec 9>"$state/lock"
flock -n 9 || { echo "une sauvegarde est déjà en cours" >&2; exit 1; }

# Essais uniquement : BACKUP_LOCAL_DIR remplace le bucket par un dossier local.
push_args=(--rm -i --env-file "$env_file")
if [ -n "${BACKUP_LOCAL_DIR:-}" ]; then
  push_args+=(-v "$BACKUP_LOCAL_DIR:/out" -e BACKUP_REMOTE=:local:/out)
fi
push() { docker run "${push_args[@]}" "$image" "$prefix/$1"; }

failed=0
step() {
  local name="$1"
  shift
  if "$@"; then
    echo "ok     $name"
  else
    echo "ÉCHEC  $name" >&2
    failed=1
  fi
}

pg() { "$compose" exec -T k8sdb sh -c "$1" sh "${@:2}"; }

dump_globals() {
  pg 'pg_dumpall -U "$POSTGRES_USER" --globals-only' | push pg-globals.sql.age
}

# Format custom : compressé, restauration base par base avec pg_restore.
dump_db() {
  pg 'pg_dump -U "$POSTGRES_USER" -Fc -d "$1"' "$1" | push "pg-$1.dump.age"
}

backup_secrets() {
  tar -C "$secrets" -czf - . | push secrets.tgz.age
}

# Volume absent (mauvais BETORUN_PROJECT) : docker run en créerait un vide, et
# la sauvegarde serait « réussie » à vide.
volume_exists() {
  docker volume inspect "${project}_$1" >/dev/null 2>&1 || { echo "volume ${project}_$1 introuvable" >&2; return 1; }
}

backup_volume() {
  volume_exists "$1" || return 1
  docker run --rm -v "${project}_$1:/data:ro" "$tools" tar -C /data -czf - . \
    | push "volume-$1.tgz.age"
}

# SeaweedFS (filer leveldb, volumes en ajout) : copie figée par docker pause,
# comme une coupure de courant, qu'il sait reprendre. La pause ne dure que la
# copie sur disque local, pas l'envoi.
paused=""
unpause() {
  if [ -n "$paused" ]; then
    "$compose" unpause seaweedfs >/dev/null || echo "ATTENTION : seaweedfs toujours en pause" >&2
    paused=""
  fi
}
cleanup() {
  unpause
  rm -f "$state/s3data.tar"
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

backup_sites() {
  local copy="$state/s3data.tar"
  rm -f "$copy"
  volume_exists s3data || return 1
  "$compose" pause seaweedfs >/dev/null || return 1
  paused=1
  local rc=0
  docker run --rm -v "${project}_s3data:/data:ro" -v "$state:/out" "$tools" \
    tar -C /data -cf /out/s3data.tar . || rc=$?
  unpause
  if [ "$rc" -eq 0 ]; then
    gzip -c "$copy" | push volume-s3data.tgz.age || rc=$?
  fi
  rm -f "$copy"
  return "$rc"
}

echo "sauvegarde $prefix"

dbs="$(pg 'psql -U "$POSTGRES_USER" -d postgres -At' <<'SQL'
SELECT datname FROM pg_database
WHERE datallowconn AND NOT datistemplate AND datname <> 'postgres'
ORDER BY 1;
SQL
)" || dbs=""
if [ -z "$dbs" ]; then
  echo "ÉCHEC  liste des bases Postgres" >&2
  failed=1
fi

step pg-globals dump_globals
for db in $dbs; do
  step "pg-$db" dump_db "$db"
done
step secrets backup_secrets
step sites backup_sites
if [ "$kind" = weekly ]; then
  step registre backup_volume registry-data
  step certificats backup_volume caddy-data
fi

if [ "$failed" -ne 0 ]; then
  echo "sauvegarde $prefix INCOMPLÈTE" >&2
  exit 1
fi
echo "$stamp $kind" > "$state/last-success"
echo "sauvegarde $prefix terminée"
