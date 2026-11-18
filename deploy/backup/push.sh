#!/bin/sh
# shellcheck shell=busybox
# push <nom> : chiffre l'entrée standard pour BACKUP_AGE_RECIPIENT et l'envoie au
# fil de l'eau sous <nom>. Rien en clair sur disque. Seule la clé publique est ici :
# ce conteneur peut chiffrer, jamais relire.
set -eu -o pipefail
name="${1:?objet manquant}"
: "${BACKUP_AGE_RECIPIENT:?}"

# BACKUP_REMOTE ne sert qu'aux essais (:local:/out) ; sinon le bucket S3.
remote="${BACKUP_REMOTE:-}"
if [ -z "$remote" ]; then
  export RCLONE_S3_PROVIDER=Other
  export RCLONE_S3_ENDPOINT="${BACKUP_S3_ENDPOINT:?}"
  export RCLONE_S3_REGION="${BACKUP_S3_REGION:?}"
  export RCLONE_S3_ACCESS_KEY_ID="${B2_KEY_ID:?}"
  export RCLONE_S3_SECRET_ACCESS_KEY="${B2_APPLICATION_KEY:?}"
  remote=":s3:${BACKUP_BUCKET:?}"
fi

# Clé en écriture seule : ni contrôle du bucket ni relecture après envoi.
# Parts de 32 Mio : flux de taille inconnue jusqu'à 320 Gio, 64 Mio de mémoire.
age -r "$BACKUP_AGE_RECIPIENT" \
  | rclone --config "" rcat \
      --s3-no-check-bucket --s3-no-head \
      --s3-chunk-size 32M --s3-upload-concurrency 2 \
      "$remote/$name"
