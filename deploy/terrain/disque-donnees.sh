#!/usr/bin/env bash
# Disque additionnel du VPS terrain : y déplacer ce qui grossit.
#
#   - volumes des conteneurs et bases (k3s local-path) : /var/lib/rancher/k3s/storage
#   - images et couches des conteneurs (containerd de k3s) : /var/lib/rancher/k3s/agent/containerd
#   - Docker (volumes de la préprod, runner de la CI)     : /var/lib/docker
#
# Le disque est formaté en ext4 (étiquette betorun-data), monté sur /mnt/data,
# et chaque dossier y est rattaché par un montage « bind » dans /etc/fstab :
# les chemins ne changent pas, rien d'autre n'est à reconfigurer.
#
# Usage (en root, sur le VPS) :
#   sudo deploy/terrain/disque-donnees.sh check            # disques vus, rien n'est modifié
#   sudo deploy/terrain/disque-donnees.sh apply /dev/sdb   # formate, copie, bascule (coupure de quelques minutes)
#   sudo deploy/terrain/disque-donnees.sh cleanup          # après vérification : supprime les anciennes copies
#
# apply refuse un disque qui contient déjà une partition, un système de
# fichiers ou un montage, et le disque du système. Les anciens dossiers sont
# gardés en *.avant-disque jusqu'à cleanup : un retour arrière reste possible
# (retirer les lignes betorun-data de /etc/fstab, renommer, redémarrer).
set -euo pipefail

MNT=/mnt/data
LABEL=betorun-data
# chemin d'origine → dossier sur le disque
DIRS=(
  "/var/lib/rancher/k3s/storage:k3s-storage"
  "/var/lib/rancher/k3s/agent/containerd:k3s-containerd"
  "/var/lib/docker:docker"
)
SUFFIX=.avant-disque

say() { printf '\n== %s\n' "$*"; }
die() { echo "✗ $*" >&2; exit 1; }
need_root() { [ "$(id -u)" = 0 ] || die "à lancer en root (sudo)"; }

# Services qui écrivent dans ces dossiers (le runner de la CI construit avec Docker).
runner_units() { systemctl list-units --type=service --no-legend 'actions.runner.*' 2>/dev/null | awk '{print $1}'; }

check() {
  say "Disques"
  lsblk -o NAME,SIZE,TYPE,FSTYPE,LABEL,MOUNTPOINT
  say "Place occupée par les dossiers à déplacer"
  for d in "${DIRS[@]}"; do
    src="${d%%:*}"
    if [ -d "$src" ]; then du -sh "$src" 2>/dev/null || echo "illisible (lancer en root) : $src"; else echo "absent : $src"; fi
  done
  say "Système"
  df -h /
  if findmnt "$MNT" >/dev/null; then echo "$MNT est déjà monté :"; findmnt "$MNT"; fi
  echo
  echo "Le nouveau disque est celui sans partition ni montage (souvent /dev/sdb)."
  echo "Ensuite : sudo $0 apply /dev/sdX"
}

apply() {
  need_root
  local dev="${1:-}"
  [ -b "$dev" ] || die "disque introuvable : $dev (voir : $0 check)"
  local root_disk
  root_disk="/dev/$(lsblk -no PKNAME "$(findmnt -no SOURCE /)")"
  [ "$dev" != "$root_disk" ] || die "$dev est le disque du système"
  [ "$(lsblk -no NAME "$dev" | wc -l)" -eq 1 ] || die "$dev contient déjà des partitions"
  [ -z "$(lsblk -no FSTYPE "$dev" | tr -d '[:space:]')" ] || die "$dev contient déjà un système de fichiers"
  [ -z "$(lsblk -no MOUNTPOINT "$dev" | tr -d '[:space:]')" ] || die "$dev est monté"
  if findmnt "$MNT" >/dev/null; then die "$MNT est déjà monté"; fi

  echo "Le disque $dev ($(lsblk -dno SIZE "$dev")) va être FORMATÉ, puis k3s, Docker et la CI seront arrêtés pendant la copie."
  read -r -p "Tapez « $dev » pour continuer : " ok
  [ "$ok" = "$dev" ] || die "annulé"

  say "Formatage et montage de $dev sur $MNT"
  mkfs.ext4 -q -L "$LABEL" "$dev"
  mkdir -p "$MNT"
  local uuid
  uuid="$(blkid -s UUID -o value "$dev")"
  cp /etc/fstab "/etc/fstab.avant-disque.$(date +%F-%H%M)"
  echo "UUID=$uuid $MNT ext4 defaults,nofail 0 2 # $LABEL" >> /etc/fstab
  mount "$MNT"

  say "Arrêt de la CI, de Docker et de k3s"
  local runners
  runners="$(runner_units)"
  if [ -n "$runners" ]; then systemctl stop $runners; fi
  systemctl stop docker.socket docker 2>/dev/null || true
  systemctl stop k3s
  if [ -x /usr/local/bin/k3s-killall.sh ]; then /usr/local/bin/k3s-killall.sh >/dev/null 2>&1 || true; fi

  say "Copie (les originaux sont gardés en *$SUFFIX)"
  for d in "${DIRS[@]}"; do
    src="${d%%:*}"; dst="$MNT/${d##*:}"
    [ -d "$src" ] || { echo "absent, ignoré : $src"; continue; }
    echo "· $src → $dst"
    mkdir -p "$dst"
    rsync -aHAXS --numeric-ids "$src/" "$dst/"
    mv "$src" "$src$SUFFIX"
    mkdir -p "$src"
    echo "$dst $src none bind,nofail,x-systemd.requires-mounts-for=$MNT 0 0 # $LABEL" >> /etc/fstab
  done
  systemctl daemon-reload
  mount -a

  say "Redémarrage de k3s, Docker et la CI"
  systemctl start k3s
  systemctl start docker
  if [ -n "$runners" ]; then systemctl start $runners; fi

  say "Vérification"
  df -h / "$MNT"
  findmnt -t ext4,none -o TARGET,SOURCE | grep -E "$MNT|rancher|docker" || true
  k3s kubectl get nodes 2>/dev/null || true
  echo
  echo "Vérifiez les conteneurs (console betorun, docker ps), puis libérez l'ancien espace : sudo $0 cleanup"
}

cleanup() {
  need_root
  findmnt "$MNT" >/dev/null || die "$MNT n'est pas monté : rien à nettoyer"
  for d in "${DIRS[@]}"; do
    src="${d%%:*}"
    findmnt "$src" >/dev/null || die "$src n'est pas sur le disque de données : cleanup refusé"
  done
  for d in "${DIRS[@]}"; do
    old="${d%%:*}$SUFFIX"
    [ -d "$old" ] || continue
    echo "· suppression de $old ($(du -sh "$old" | cut -f1))"
    rm -rf --one-file-system "$old"
  done
  df -h /
}

case "${1:-}" in
  check) check ;;
  apply) shift; apply "$@" ;;
  cleanup) cleanup ;;
  *) sed -n '2,21p' "$0"; exit 1 ;;
esac
