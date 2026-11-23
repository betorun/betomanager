#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Bascule SUR PLACE d'un k3s mono-nœud de flannel vers Cilium (ADR 0014).
# Les données (disques local-path, secrets, objets) sont gardées ; les pods
# sont recréés sur le nouveau réseau : coupure de quelques minutes.
#
#   sudo bash cilium-migrate.sh check      # état et réponses des sites (rien ne change)
#   sudo bash cilium-migrate.sh migrate    # bascule (demande confirmation)
#   sudo bash cilium-migrate.sh rollback   # retour à flannel
#
# Variables : K3S_IP (défaut : IP du nœud), CILIUM_VERSION (défaut : version
# stable du CLI), RBAC (défaut : deploy/k8s/rbac.yaml à côté de ce script).
# Journal et relevés : /root/cilium-migrate/<date>/
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "lancer avec sudo"; exit 1; }

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
HERE="$(cd "$(dirname "$0")" && pwd)"
RBAC="${RBAC:-$HERE/rbac.yaml}"
CONF=/etc/rancher/k3s/config.yaml
DIR="/root/cilium-migrate/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$DIR"
exec > >(tee -a "$DIR/journal.txt") 2>&1

say() { printf '\n── %s\n' "$*"; }
die() { echo "ÉCHEC : $*"; echo "Journal : $DIR"; exit 1; }

node_ip() { kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'; }

wait_api() {
  for _ in $(seq 1 90); do kubectl get --raw=/readyz >/dev/null 2>&1 && return 0; sleep 2; done
  die "l'API k3s ne répond pas"
}

# Relevé : pods, et code HTTP de chaque hôte d'ingress (via le point d'entrée local).
snapshot() {
  local tag="$1"
  kubectl get nodes -o wide > "$DIR/$tag-nodes.txt"
  kubectl get pods -A -o wide > "$DIR/$tag-pods.txt"
  kubectl get ingress -A -o jsonpath='{range .items[*]}{range .spec.rules[*]}{.host}{"\n"}{end}{end}' \
    | sort -u | grep -v '^$' > "$DIR/$tag-hosts.txt" || true
  : > "$DIR/$tag-http.txt"
  while read -r h; do
    code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 --resolve "$h:443:127.0.0.1" "https://$h/" || echo 000)
    echo "$code $h" >> "$DIR/$tag-http.txt"
  done < "$DIR/$tag-hosts.txt"
  local bad
  bad=$(kubectl get pods -A --no-headers | awk '$4!="Running" && $4!="Completed" {print}' | wc -l)
  echo "pods hors Running/Completed : $bad ; hôtes relevés : $(wc -l < "$DIR/$tag-hosts.txt")"
}

# Pods à recréer : tous sauf ceux du réseau de l'hôte (ils n'ont pas d'IP de pod).
restart_pods() {
  kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{" "}{.spec.hostNetwork}{"\n"}{end}' \
    | awk '$3!="true" {print $1, $2}' | while read -r ns name; do
      kubectl -n "$ns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
    done
}

# Pods hors service : on attend d'en avoir au plus autant qu'avant la bascule
# (BASELINE_BAD) ; ceux déjà cassés avant (image introuvable…) le restent.
BASELINE_BAD=0
count_bad() { kubectl get pods -A --no-headers 2>/dev/null | awk '$4!="Running" && $4!="Completed"' | wc -l; }
wait_pods() {
  for _ in $(seq 1 60); do
    [ "$(count_bad)" -le "$BASELINE_BAD" ] && return 0
    sleep 10
  done
  echo "ATTENTION : des pods ne sont pas revenus :"
  kubectl get pods -A --no-headers | awk '$4!="Running" && $4!="Completed"'
}

compare_http() {
  say "réponses des sites : avant → après"
  join -1 2 -2 2 <(sort -k2 "$DIR/avant-http.txt") <(sort -k2 "$DIR/apres-http.txt") \
    | awk '{flag = ($2==$3) ? "" : "   ← CHANGÉ"; printf "%-50s %s → %s%s\n", $1, $2, $3, flag}'
}

install_cilium_cli() {
  command -v cilium >/dev/null && return 0
  say "CLI cilium"
  local v arch=amd64
  v=$(curl -s https://raw.githubusercontent.com/cilium/cilium-cli/main/stable.txt)
  curl -sL --fail -o /tmp/cilium.tgz "https://github.com/cilium/cilium-cli/releases/download/${v}/cilium-linux-${arch}.tar.gz"
  curl -sL --fail -o /tmp/cilium.tgz.sha256sum "https://github.com/cilium/cilium-cli/releases/download/${v}/cilium-linux-${arch}.tar.gz.sha256sum"
  (cd /tmp && sed 's#cilium-linux-amd64.tar.gz#cilium.tgz#' cilium.tgz.sha256sum | sha256sum -c -) || die "somme de contrôle du CLI cilium"
  tar -C /usr/local/bin -xzf /tmp/cilium.tgz cilium
  rm -f /tmp/cilium.tgz /tmp/cilium.tgz.sha256sum
}

cmd_check() {
  say "état actuel"
  wait_api
  snapshot avant
  if kubectl -n kube-system get ds cilium >/dev/null 2>&1; then echo "Cilium est DÉJÀ installé."; else echo "Réseau actuel : flannel (Cilium absent)."; fi
  echo "Relevés : $DIR"
}

cmd_migrate() {
  wait_api
  kubectl -n kube-system get ds cilium >/dev/null 2>&1 && die "Cilium est déjà installé"
  [ "$(kubectl get nodes --no-headers | wc -l)" -eq 1 ] || die "ce script ne vise qu'un cluster à un seul nœud"
  local ip="${K3S_IP:-$(node_ip)}"
  [ -n "$ip" ] || die "IP du nœud introuvable (K3S_IP=…)"

  say "1/7 relevé avant"
  snapshot avant
  BASELINE_BAD=$(count_bad)
  echo
  read -r -p "Basculer ce cluster sur Cilium (coupure de quelques minutes) ? Taper OUI : " ok
  [ "$ok" = "OUI" ] || die "annulé"

  say "2/7 sauvegarde de la configuration k3s"
  [ -f "$CONF" ] && cp -a "$CONF" "$DIR/config.yaml.avant" || : > "$DIR/config.yaml.avant.absent"
  iptables-save > "$DIR/iptables.avant"

  install_cilium_cli

  say "3/7 k3s sans flannel, sans kube-proxy ni contrôleur de règles réseau"
  mkdir -p "$(dirname "$CONF")"
  touch "$CONF"
  grep -q '^flannel-backend:' "$CONF" || echo 'flannel-backend: none' >> "$CONF"
  grep -q '^disable-network-policy:' "$CONF" || echo 'disable-network-policy: true' >> "$CONF"
  grep -q '^disable-kube-proxy:' "$CONF" || echo 'disable-kube-proxy: true' >> "$CONF"
  cat "$CONF"
  systemctl restart k3s
  wait_api

  say "4/7 restes de flannel et de kube-proxy"
  ip link delete flannel.1 2>/dev/null || true
  ip link delete cni0 2>/dev/null || true
  # Règles KUBE-* et FLANNEL-* seulement : celles du pare-feu (ufw) restent.
  iptables-save | grep -v -E 'KUBE-|FLANNEL' | iptables-restore
  ip6tables-save | grep -v -E 'KUBE-|FLANNEL' | ip6tables-restore || true

  say "5/7 Cilium (passerelle de sortie, remplacement de kube-proxy)"
  # shellcheck disable=SC2086
  cilium install ${CILIUM_VERSION:+--version "$CILIUM_VERSION"} \
    --set kubeProxyReplacement=true \
    --set k8sServiceHost="$ip" --set k8sServicePort=6443 \
    --set ipam.mode=kubernetes \
    --set egressGateway.enabled=true --set bpf.masquerade=true \
    --set operator.replicas=1
  cilium status --wait --wait-duration 10m || die "Cilium ne s'est pas mis en route (rollback possible)"

  say "6/7 pods recréés sur le nouveau réseau"
  restart_pods
  sleep 20
  wait_pods
  [ -f "$RBAC" ] && kubectl apply -f "$RBAC" || echo "rbac.yaml introuvable ($RBAC) : le réappliquer à la main"

  say "7/7 relevé après"
  snapshot apres
  compare_http
  echo
  echo "Adresse de l'hôte vue des pods (tunnels « ssh -R » : remplace 10.42.0.1) :"
  ip -4 -o addr show cilium_host 2>/dev/null | awk '{print "  " $4}'
  echo
  echo "Terminé. Journal et relevés : $DIR"
  echo "Retour arrière si besoin : sudo bash $0 rollback"
}

cmd_rollback() {
  say "retour à flannel"
  wait_api
  BASELINE_BAD=$(count_bad)
  local last
  last=$(ls -1d /root/cilium-migrate/*/ 2>/dev/null | while read -r d; do [ -e "$d/config.yaml.avant" ] || [ -e "$d/config.yaml.avant.absent" ] && echo "$d"; done | tail -1)
  [ -n "$last" ] || die "aucune sauvegarde de configuration trouvée"
  read -r -p "Revenir à flannel (sauvegarde $last) ? Taper OUI : " ok
  [ "$ok" = "OUI" ] || die "annulé"
  command -v cilium >/dev/null && cilium uninstall --wait || true
  if [ -e "$last/config.yaml.avant" ]; then cp -a "$last/config.yaml.avant" "$CONF"; else rm -f "$CONF"; fi
  ip link delete cilium_host 2>/dev/null || true
  ip link delete cilium_vxlan 2>/dev/null || true
  iptables-save | grep -v -E 'CILIUM' | iptables-restore
  systemctl restart k3s
  wait_api
  restart_pods
  sleep 20
  wait_pods
  snapshot apres-rollback
  echo "Revenu à flannel. Journal : $DIR"
}

case "${1:-}" in
  check) cmd_check ;;
  migrate) cmd_migrate ;;
  rollback) cmd_rollback ;;
  *) echo "usage : sudo bash $0 check|migrate|rollback"; exit 2 ;;
esac
