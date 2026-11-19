# Secrets management — politique KubeManager

> Règle d'or : **aucun secret en clair n'est committé dans ce dépôt**. Tous les chemins de prod ci-dessous sont chiffrés au repos et rotatables sans rebuild d'image.

## Clés attendues par le chart

Le chart `chart/kubemanager-backend` consomme un K8s Secret référencé par `.Values.existingSecret` (défaut `kubemanager-secrets`) avec les clés :

| Clé | Usage |
|-----|-------|
| `DB_PASSWORD` | Mot de passe Postgres (utilisateur `kubemanager`) |
| `REDIS_PASSWORD` | Mot de passe Redis (optionnel) |
| `JWT_SECRET` | Signature JWT access tokens (HS256) |
| `JWT_REFRESH_SECRET` | Signature JWT refresh tokens (HS256) |
| `KEYCLOAK_ADMIN_CLIENT_SECRET` | Secret du client admin Keycloak (admin REST API) |
| `CENTRIFUGO_API_KEY` | API key pour POST `/publish` vers Centrifugo |
| `CENTRIFUGO_TOKEN_HMAC_SECRET` | HMAC pour signer les JWT Centrifugo (doit matcher côté broker) |

## Trois stratégies, par environnement

### 1. Dev local — env vars + docker-compose

Les secrets vivent dans `docker-compose.yaml` et variables d'env locales. Suffisant tant que la machine dev ne tourne pas en multi-user.

### 2. Staging — envsubst + kubectl (transition)

Template : `chart/kubemanager-backend/examples/secret-plain.yaml`.

```sh
export KUBEMANAGER_DB_PASSWORD=...
export KUBEMANAGER_JWT_SECRET=$(openssl rand -hex 32)
# etc.
envsubst < chart/kubemanager-backend/examples/secret-plain.yaml \
  | kubectl apply -n kubemanager -f -
```

Acceptable pour un env temporaire. **Pas pour prod** : pas de rotation, pas d'audit.

### 3. Prod — ExternalSecrets + Vault (recommandé)

Template : `chart/kubemanager-backend/examples/externalsecret.yaml`.

Pipeline :

```
Vault (source de vérité, rotation, audit)
   │
   ▼
external-secrets operator (reconcile 1h)
   │
   ▼
K8s Secret `kubemanager-secrets` (namespace kubemanager)
   │
   ▼
Pods kubemanager-backend (envFrom)
```

**Opérations courantes** :

- Rotation d'un secret : update dans Vault → ESO pull sous 1h → rolling restart automatique via une annotation `checksum/secrets` (TODO: activer la reload sur Secret change via Reloader ou kubectl rollout restart).
- Audit : logs Vault (qui a lu quoi, quand).
- Break-glass : accès direct Vault via CLI avec policy `kubemanager-admin`.

**Variantes acceptées** : AWS Secrets Manager, GCP Secret Manager, Doppler — changer la `ClusterSecretStore` dans l'exemple.

### 4. Prod sans Vault — SOPS + age (alternative GitOps)

Si pas d'infra Vault, SOPS permet de committer des secrets chiffrés dans le repo (GitOps-friendly).

```sh
# Chiffrer
sops --age age1... -e secret-plain.yaml > deploy/secrets/kubemanager.enc.yaml

# Déchiffrer + apply en CI
sops -d deploy/secrets/kubemanager.enc.yaml | kubectl apply -n kubemanager -f -
```

`.sops.yaml` au root du dépôt définit les `creation_rules` (quel fichier → quelle recipient age / KMS). Voir `chart/kubemanager-backend/examples/sops.example.enc.yaml`.

## Rotation — procédure standard

1. Générer le nouveau secret dans Vault (`vault kv put prod/kubemanager/jwt access=<new>`).
2. ExternalSecrets syncront sous `refreshInterval` (1h par défaut).
3. Forcer le redéploiement des pods : `kubectl -n kubemanager rollout restart deploy/kubemanager-server deploy/kubemanager-worker`.
4. Vérifier `kubectl logs` — aucun 401/403 en cascade.
5. Pour JWT : tolérance pendant la rotation = durée de vie du token (config `jwt.access_expiration`). Double-validation côté middleware n'est pas implémentée — planifier `kubectl rollout restart` pendant un creux de trafic.

## Garde-fous

- **Git pre-commit hook** : interdire `*.env`, `secret*.yaml` non-encrypted. Utiliser `detect-secrets` ou `gitleaks`.
- **CI** : le job de build doit refuser de packager un chart qui embed un Secret stringData — `helm lint --strict`.
- **Pod logs** : `zap` redact par défaut les champs nommés `password|token|secret|apikey` — vérifier via `scripts/test-redact.sh` (à écrire).
- **RBAC K8s** : seul le ServiceAccount `kubemanager-backend` peut lire `kubemanager-secrets`. Vérifier via `kubectl auth can-i get secret kubemanager-secrets -n kubemanager --as=system:serviceaccount:default:default` → `no`.

## Références

- ExternalSecrets : <https://external-secrets.io>
- SOPS : <https://github.com/getsops/sops>
- Reloader (rolling-restart sur Secret change) : <https://github.com/stakater/Reloader>
