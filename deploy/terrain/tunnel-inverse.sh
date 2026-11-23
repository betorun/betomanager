#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Tunnel inversé PC → VPS, visible des pods UNIQUEMENT (pas d'Internet).
# À lancer sur le serveur (rejouable) :   sudo bash deploy/terrain/tunnel-inverse.sh
#
# Les pods joignent l'API et le registre de ton PC sur l'adresse de l'hôte vue
# des pods (GW), qu'Internet ne voit pas :
#   - sous Cilium (terrain depuis le 2026-11-23) : l'IP de cilium_host
#     (10.42.0.109 sur le terrain) ;
#   - sous flannel (avant) : 10.42.0.1, le pont cni0.
#   http://GW:18080   → API du PC (127.0.0.1:18080)
#   GW:15000          → registre du PC (127.0.0.1:5000), en HTTP
#   localhost:18080   → API du PC, côté serveur : containerd y demande le jeton
#                       du registre (realm http://127.0.0.1:18080/…/registry/token)
#
# Côté PC, une seule commande ssh pour tout (6443 + les retours), GW remplacé :
#   ssh -N -o ExitOnForwardFailure=yes -p 49212 -L 127.0.0.1:6443:127.0.0.1:6443 \
#       -R GW:18080:127.0.0.1:18080 -R localhost:18080:127.0.0.1:18080 \
#       -R GW:15000:127.0.0.1:5000 sykdev@51.75.141.233
#   (PermitListen compare le nom tel qu'il est écrit : « localhost », pas « 127.0.0.1 ».)
#
# API du PC : les pods la joignent par GW, à déclarer dans sa configuration :
#   KUBEMANAGER_BUILDS_PUBLIC_API_URL=http://GW:18080/kmanager/v1
#   KUBEMANAGER_BUILDS_EGRESS_ALLOW_CIDRS=GW/32
#   KUBEMANAGER_BUILDS_REGISTRY_HOST=GW:15000
#
# Ce que ça change sur le serveur :
#   1. sshd : GatewayPorts clientspecified, écoutes distantes limitées à GW:18080,
#      GW:15000 et localhost — rien ne peut écouter sur l'IP publique ;
#   2. ufw : les pods (10.42.0.0/16) joignent l'hôte, règle EN TÊTE (ufw applique
#      la première qui correspond ; sinon « deny 6443 » coupe aussi l'API aux pods) ;
#   3. k3s : registre en HTTP (registries.yaml) sous GW:15000 et sous l'ancien
#      nom 10.42.0.1:15000 (images construites avant Cilium), redémarrage k3s.
# Sous Cilium, les règles réseau par adresse (EGRESS_ALLOW_CIDRS) ne visent l'hôte
# qu'avec « policyCIDRMatchMode=nodes » (posé sur le terrain le 2026-11-23).
#
# Retour arrière : les fichiers modifiés sont copiés en *.avant-tunnel ;
# supprimer la règle ufw des pods (ufw status numbered), reload ssh, restart k3s.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "lancer avec sudo"; exit 1; }

LEGACY=10.42.0.1                     # nom des images construites sous flannel
PODS=10.42.0.0/16
SSHD_CONF=/etc/ssh/sshd_config.d/40-betorun-reverse.conf
REGISTRIES=/etc/rancher/k3s/registries.yaml

# Adresse de l'hôte vue des pods : cilium_host (Cilium) ou cni0 (flannel).
GW=$(ip -4 -o addr show cilium_host 2>/dev/null | awk '{split($4, a, "/"); print a[1]; exit}')
if [ -z "$GW" ]; then
  GW=$(ip -4 -o addr show cni0 2>/dev/null | awk '{split($4, a, "/"); print a[1]; exit}')
fi
[ -n "$GW" ] || { echo "ni cilium_host ni cni0 : k3s démarré ?"; exit 1; }
echo "Adresse de l'hôte vue des pods : ${GW}"

backup() { [ -f "$1" ] && [ ! -f "$1.avant-tunnel" ] && cp -a "$1" "$1.avant-tunnel" || true; }

echo "── 1. sshd"
backup "$SSHD_CONF"
cat > "$SSHD_CONF" <<EOF
# betorun terrain : retours PC → pods (voir deploy/terrain/tunnel-inverse.sh)
GatewayPorts clientspecified
PermitListen ${GW}:18080 ${GW}:15000 localhost:*
EOF
sshd -t
systemctl reload ssh
sshd -T 2>/dev/null | grep -E '^(gatewayports|permitlisten)'

echo "── 2. ufw"
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw status | grep -v "on cni0" | grep -q "ALLOW.*${PODS}" || ufw insert 1 allow from "${PODS}" comment "pods k3s"
  # Règles de l'époque flannel (interface cni0, disparue sous Cilium).
  while n=$(ufw status numbered | grep -E "on cni0" | head -1 | sed -E 's/^\[ *([0-9]+)\].*/\1/'); [ -n "$n" ]; do
    ufw --force delete "$n" >/dev/null
  done
  ufw status numbered | head -6
else
  echo "ufw inactif : rien à faire"
fi

echo "── 3. k3s : registre en HTTP"
backup "$REGISTRIES"
{
  echo "mirrors:"
  echo "  \"${GW}:15000\":"
  echo "    endpoint:"
  echo "      - \"http://${GW}:15000\""
  if [ "$GW" != "$LEGACY" ]; then
    echo "  \"${LEGACY}:15000\":"
    echo "    endpoint:"
    echo "      - \"http://${GW}:15000\""
  fi
} > "$REGISTRIES"
cat "$REGISTRIES"
systemctl restart k3s
until KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl get nodes >/dev/null 2>&1; do sleep 3; done

echo
echo "OK. Depuis ton PC :"
echo "  ssh -N -o ExitOnForwardFailure=yes -p 49212 -L 127.0.0.1:6443:127.0.0.1:6443 \\"
echo "      -R ${GW}:18080:127.0.0.1:18080 -R localhost:18080:127.0.0.1:18080 \\"
echo "      -R ${GW}:15000:127.0.0.1:5000 sykdev@51.75.141.233"
echo "Et dans la configuration de l'API du PC :"
echo "  KUBEMANAGER_BUILDS_PUBLIC_API_URL=http://${GW}:18080/kmanager/v1"
echo "  KUBEMANAGER_BUILDS_EGRESS_ALLOW_CIDRS=${GW}/32"
echo "  KUBEMANAGER_BUILDS_REGISTRY_HOST=${GW}:15000"
