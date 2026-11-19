# Rotation d'une cluster key

> Les API keys `kind=cluster` sont utilisées par les watchers K8s distants. Rotation = générer une nouvelle clé, laisser l'ancienne active un temps (grace period), déployer la nouvelle sur le watcher.

## Quand le faire

- **Routine** : tous les 90 jours (règle sec).
- **Urgence** : suspicion de fuite (clé en clair dans un log, un screenshot, une commit).
- **Départ collaborateur** : si l'admin qui a créé la clé quitte l'équipe.

## Procédure

### 1. Identifier la clé à rotate

```sh
TOKEN=$(bash scripts/keycloak/get_test_token.sh)

curl -sS -H "Authorization: Bearer $TOKEN" \
  https://api.kubemanager/kmanager/v1/admin/cluster-keys | jq
```

Réponse (extrait) :

```json
[
  {
    "id": "abc-123-uuid",
    "cluster_id": "prod-fr-north-1",
    "prefix": "km_clus_XXX",
    "status": "active",
    "created_at": "2026-01-18T...",
    "expires_at": null
  }
]
```

Noter le `id` de la clé.

### 2. Rotate

```sh
KEY_ID=abc-123-uuid
GRACE_DAYS=7

curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  https://api.kubemanager/kmanager/v1/admin/cluster-keys/$KEY_ID/rotate \
  -d "{\"grace_days\": $GRACE_DAYS}" | jq
```

Réponse :

```json
{
  "id": "new-uuid-456",
  "cluster_id": "prod-fr-north-1",
  "prefix": "km_clus_YYY",
  "api_key": "km_clus_YYY_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "created_at": "2026-04-18T..."
}
```

⚠️ **Le champ `api_key` (token clair) n'est retourné QUE maintenant** — copie-le dans Vault / 1Password immédiatement, il ne sera plus accessible.

Effet en DB :
- Nouvelle row `api_keys` avec `cluster_id=prod-fr-north-1, is_active=true, expires_at=NULL`.
- Ancienne row : `expires_at = NOW() + grace_days × 24h`. Elle reste `is_active=true` pendant la grace period.

### 3. Déployer la nouvelle clé sur le watcher

Le watcher tourne via le chart Helm `watcher/chart/`. Upgrade avec la nouvelle clé :

```sh
helm -n kubemanager-watcher upgrade watcher \
  /path/to/watcher/chart \
  --set kubemanager.apiKey="km_clus_YYY_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" \
  --reuse-values
```

Ou via ExternalSecrets : update la valeur dans Vault, ESO sync sous 1 h, puis restart :

```sh
kubectl -n kubemanager-watcher rollout restart deploy/watcher
```

### 4. Vérifier que le watcher utilise la nouvelle clé

```sh
kubectl -n kubemanager-watcher logs -l app=watcher --tail=50
```

Cherche une ligne `enroll request accepted` ou `onPodEvent sent successfully`. Si 401, la nouvelle clé n'est pas prise — check le Secret K8s :

```sh
kubectl -n kubemanager-watcher get secret watcher-secrets \
  -o jsonpath='{.data.API_KEY}' | base64 -d
```

Doit matcher ta nouvelle clé.

### 5. (Optionnel) Révoquer l'ancienne immédiatement

Si **pas de grace period** souhaitée (ex: fuite confirmée) :

```sh
OLD_KEY_ID=abc-123-uuid

curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://api.kubemanager/kmanager/v1/users/api-keys/$OLD_KEY_ID
```

Le watcher qui essaie encore d'utiliser l'ancienne → 401 immédiat.

### 6. Vérifier la santé côté kubemanager

Dashboard Grafana → "KubeManager — Overview" :
- L'alerte `ClusterWatcherStale` ne doit PAS se déclencher après la rotation.
- `kubemanager_cluster_last_seen_timestamp_seconds{cluster_id="prod-fr-north-1"}` doit se mettre à jour dans les 60 s.

## Cas particuliers

### Plusieurs watchers sur le même cluster_id

Normalement impossible (leader election via Lease). Si ça arrive, les deux watchers partagent l'API key → la rotation propre nécessite de redéployer les deux simultanément.

### Perte du token clair avant de l'avoir injecté

Impossible de le récupérer (BCrypt irréversible). Il faut :

1. Rotate à nouveau → nouveau token.
2. Révoquer la clé "intermédiaire" (celle qu'on vient de générer mais jamais utilisée).

### Grace period déjà expirée mais watcher pas encore update

L'ancienne clé est rejetée → watcher reçoit 401 → `ClusterWatcherStale` alert → oncall intervient.

Recours :

- Si urgent : créer une **nouvelle** clé via `POST /admin/cluster-keys`, la déployer sur le watcher.
- Si sauvable : rallonger l'expiration via SQL (bypass audit, à n'utiliser qu'en dernier recours) :

```sql
UPDATE api_keys
SET expires_at = NOW() + INTERVAL '24 hours', is_active = true
WHERE id = '<old-id>';
```

## Grace period — recommandations

| Cas | `grace_days` |
|-----|--------------|
| Rotation routine, watcher OpsStable | 7 |
| Rotation post-incident (fuite) | 0 (immédiate) |
| Cluster au fuseau horaire lointain, update manuel | 14 |

## Audit trail

Chaque rotation est loggée :

```sql
SELECT * FROM api_keys
WHERE cluster_id = 'prod-fr-north-1'
ORDER BY created_at DESC;
```

Voir aussi les logs middleware — chaque usage de la clé incrémente `usage_count` + update `last_used_at`. Utile pour vérifier qu'après rotation, l'ancienne clé n'est plus utilisée.

## Checklist post-rotation

- [ ] Nouvelle clé stockée dans Vault / 1Password.
- [ ] Watcher a redéployé avec la nouvelle clé.
- [ ] Logs watcher : pas de 401.
- [ ] Grafana : `cluster_last_seen_timestamp_seconds` à jour.
- [ ] (Après `grace_days`) : ancienne clé auto-expirée, confirmé via `SELECT * FROM api_keys WHERE expires_at < NOW()`.
- [ ] Ticket d'audit créé (si post-incident).
