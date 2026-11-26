#!/bin/sh
# Réseau privé du projet : inscription de ce VPS (à lancer en root).
# La clé du VPS est générée ici et n'en sort pas : seule la clé publique est
# envoyée, avec un jeton d'inscription à usage unique.
set -eu
API='__API__'
TOKEN="${1:-__TOKEN__}"
IF=vpc0
DIR=/etc/wireguard
KEY="$DIR/$IF.key"
CONF="$DIR/$IF.conf"
log() { echo "vpc: $*" >&2; }

[ "$(id -u)" = 0 ] || { log "à lancer en root (sudo)"; exit 1; }
case "$TOKEN" in
  bvpc_*) ;;
  *) log "jeton d'inscription manquant"; exit 1 ;;
esac

if ! command -v wg >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || { log "système non pris en charge (apt-get absent)"; exit 1; }
  export DEBIAN_FRONTEND=noninteractive
  # Au premier démarrage, apt peut être occupé (cloud-init) : on réessaie.
  i=0
  until apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq wireguard-tools curl iproute2 >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 20 ] || { log "installation des outils impossible"; exit 1; }
    sleep 15
  done
fi

umask 077
mkdir -p "$DIR"
[ -s "$KEY" ] || wg genkey >"$KEY"
PUB=$(wg pubkey <"$KEY")

RESP=$(curl -4 -fsS --retry 20 --retry-delay 15 --retry-connrefused --max-time 30 \
  -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"public_key\":\"$PUB\"}" "$API/vpc/enroll") || {
  log "inscription refusée (jeton utilisé ou expiré) ou API injoignable"
  exit 1
}

field() { printf '%s' "$RESP" | tr -d '\n' | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"; }
ADDR=$(field address)
ENDPOINT=$(field endpoint)
PEER=$(field peer_public_key)
ALLOWED=$(field allowed_ips)
KEEPALIVE=$(field keepalive)
if [ -z "$ADDR" ] || [ -z "$ENDPOINT" ] || [ -z "$PEER" ] || [ -z "$ALLOWED" ]; then
  log "réponse inattendue de l'API"
  exit 1
fi

cat >"$CONF" <<CONF
[Interface]
PrivateKey = $(cat "$KEY")
Address = $ADDR/32

[Peer]
PublicKey = $PEER
Endpoint = $ENDPOINT
AllowedIPs = $ALLOWED
PersistentKeepalive = ${KEEPALIVE:-25}
CONF
chmod 600 "$CONF"
systemctl enable "wg-quick@$IF" >/dev/null 2>&1
systemctl restart "wg-quick@$IF"
log "VPS relié au réseau privé du projet ($ADDR)"
