#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Terrain d'essai betorun : k3s sur ce VPS (51.75.141.233).
# À lancer UNE fois, depuis le dépôt, sur le serveur :
#   sudo ACME_MAIL=... -E bash deploy/terrain/k3s.sh
#
# Installe k3s avec flannel. Le terrain est passé sous Cilium le 2026-11-23
# (deploy/k8s/cilium-migrate.sh, puis deploy/terrain/tunnel-inverse.sh) ;
# pour un nouveau serveur, lancer ces deux scripts après celui-ci.
#
# Ce que ça fait :
#   1. pare-feu : SSH (port 49212 !), 80, 443 ; 6443 (API k3s) FERMÉ au public ;
#   2. k3s v1.36.5 (version du guide) + Traefik intégré, secrets chiffrés ;
#   3. cert-manager v1.21.3 + émetteur letsencrypt-prod ;
#   4. compte de la plateforme à droits réduits (rbac.yaml) ;
#   5. kubeconfig restreint → $OUT (défaut ~/kubeconfig.terrain, 600 ; JAMAIS dans git) ;
#
# Accès depuis ton PC, sans ouvrir 6443 (tunnel par ton compte habituel) :
#   ssh -N -p 49212 -L 6443:127.0.0.1:6443 sykdev@51.75.141.233
#   KUBECONFIG=kubeconfig.terrain kubectl get ns      # server = 127.0.0.1:6443
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "lancer avec sudo"; exit 1; }

IP=51.75.141.233
SSH_PORT=49212                       # PAS 22 : « ufw allow OpenSSH » te bloquerait dehors
DEPLOY="$(cd "$(dirname "$0")/../k8s" && pwd)"
OWNER="${SUDO_USER:-root}"
OUT="${OUT:-$(getent passwd "$OWNER" | cut -d: -f6)/kubeconfig.terrain}"
ACME_MAIL="${ACME_MAIL:?ACME_MAIL=ton-adresse@... sudo -E bash $0}"

echo "── 1. pare-feu"
ufw allow ${SSH_PORT}/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw deny 6443/tcp
ufw --force enable
ufw status verbose | head -20

echo "── 2. k3s"
if ! command -v k3s >/dev/null; then
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='v1.36.5+k3s1' \
    INSTALL_K3S_EXEC="server --secrets-encryption --tls-san ${IP} --tls-san 127.0.0.1 --write-kubeconfig-mode 600" sh -
fi
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
until kubectl get nodes >/dev/null 2>&1; do sleep 3; done
# Traefik est posé par un Job Helm de k3s, une à deux minutes après le démarrage.
echo "attente du déploiement de Traefik…"
until kubectl -n kube-system get deploy/traefik >/dev/null 2>&1; do sleep 5; done
kubectl -n kube-system rollout status deploy/traefik --timeout=300s
k3s secrets-encrypt status | grep -i 'encryption status'

echo "── 3. cert-manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=300s
sed "s/ops@REMPLACER.invalid/${ACME_MAIL}/" "${DEPLOY}/cluster-issuer.yaml" | kubectl apply -f -

echo "── 4. compte plateforme"
kubectl apply -f "${DEPLOY}/rbac.yaml"
until kubectl -n betorun-system get secret betorun-platform-token -o jsonpath='{.data.token}' 2>/dev/null | grep -q .; do sleep 2; done

echo "── 5. kubeconfig restreint (via tunnel → 127.0.0.1)"
umask 077
bash "${DEPLOY}/make-kubeconfig.sh" 127.0.0.1 > "${OUT}"
chown "${OWNER}:" "${OUT}"

echo
echo "OK. Reste à faire :"
echo "  - copier ${OUT} sur ton PC (scp -P ${SSH_PORT}), ouvrir le tunnel, kubectl get ns"
echo "  - AppArmor : kernel.apparmor_restrict_unprivileged_userns=1 ici ; si un build BuildKit"
echo "    échoue au démarrage, c'est la première piste (README deploy §4)."
