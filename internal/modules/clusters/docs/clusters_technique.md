# Module `clusters`

> Enregistre les clusters K8s distants via un watcher embarqué (projet séparé `betorun/watcher`). Protocole Restate-compatible : Enroll → GetConfig → OnPodEvent.

## Endpoints watcher (protocole)

Tous sous `/clusters/*` — protégés par guard `kind="cluster"` (API key avec scope `cluster:report`).

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/clusters/ClusterService/Enroll` | Enregistrement au démarrage du watcher. Upsert DB ; retourne `{status: "enrolled"\|"updated", cluster_id, at}` |
| `GET`  | `/clusters/ConfigService/:clusterID/Get` | Retourne la config remote (dedup_cooldown, batch_size, enabled_reasons…) |
| `POST` | `/clusters/ProjectService/:clusterID/onPodEvent` | Reçoit un pod event, map sur le workload, update DB, publish Centrifugo |

Déclarations : `internal/infrastructure/router/route.go:35-45`.

## Admin

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `GET` | `/admin/clusters` | Liste tous les clusters avec `{ClusterID, Version, LastSeenAt, EnrolledAt}` |

Protection : `RequireGlobalRole(SUPER_ADMIN, ADMIN)`.

## Controller

```go
type IClusterController interface {
    Enroll(c *gin.Context, in *EnrollRequest) (*EnrollResponse, error)
    GetConfig(c *gin.Context, in *ClusterIDPath) (*RemoteConfigResponse, error)
    OnPodEvent(c *gin.Context, in *PodEventInput) (*PodEventAck, error)
    ListClusters(c *gin.Context) ([]Cluster, error)
}
```

Fichier : `internal/modules/clusters/hclusters.go`.

## Persistance — table `clusters`

```sql
CREATE TABLE clusters (
  id           UUID PRIMARY KEY,
  cluster_id   VARCHAR(100) UNIQUE NOT NULL, -- identifiant logique (ex: prod-fr-north-1)
  version      VARCHAR(50),                   -- version du watcher
  last_seen_at TIMESTAMPTZ,                   -- touché par onPodEvent
  enrolled_at  TIMESTAMPTZ NOT NULL,          -- premier Enroll, immutable
  created_at   TIMESTAMPTZ NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX ON clusters(last_seen_at);
```

Fichier entité : `internal/modules/clusters/entity.go`.

## Service

- **Enroll** : upsert ; distingue `enrolled` vs `updated` selon `EnrolledAt == CreatedAt`.
- **GetConfig** : retourne un `defaultRemoteConfig` hardcodé (évolution future = table `clusters_config`).
- **OnPodEvent** :
  1. Extrait le label `km-workload-id` du pod.
  2. Lookup du workload par ID (cross-check `project_id`).
  3. Map la `reason` K8s (CrashLoopBackOff, ImagePullBackOff…) sur un status interne.
  4. UPDATE du `workloads.status` en DB.
  5. Publish Centrifugo sur `project:X:workload:Y:deploy` + `project:X:events`.
  6. Retourne `PodEventAck{event_id, status: "mapped"\|"accepted"\|"ignored", workload_id?}`.
- **ListClusters** : admin view pour détection staleness.

## Config remote (exemple)

```json
{
  "dedup_cooldown_seconds": 60,
  "batch_size": 50,
  "batch_flush_ms": 1000,
  "enabled_reasons": [
    "CrashLoopBackOff",
    "ImagePullBackOff",
    "OOMKilled",
    "Evicted",
    "Failed",
    "Unhealthy"
  ]
}
```

## Sécurité

- Guard `kind="cluster"` appliqué au groupe `/clusters` entier (route.go:31-32).
- API key créée par admin via `POST /admin/cluster-keys` → injectée dans `values.yaml` du chart watcher.
- Rotation : `POST /admin/cluster-keys/:id/rotate` avec grace period (défaut 7j).

## Watcher (projet externe)

Le projet `betorun/watcher` :
- Leader election via Lease K8s (un seul pod actif par cluster).
- Informers pods + events (client-go).
- Dédup par `(namespace/pod/reason)` avec TTL `dedup_cooldown_seconds`.
- Batch flush toutes les `batch_flush_ms`.
- HTTP POST vers kubemanager avec header `X-API-Key: <cluster-key>`.
- Helm chart inclus : `betorun/watcher/chart/` (NOTES.txt + templates).
