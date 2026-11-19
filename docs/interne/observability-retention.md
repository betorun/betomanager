# Rétention Prometheus & Loki — politique KubeManager

> Objectif : couvrir **les cycles d'audit trimestriels** sans gaspiller du stockage. 30 jours chaud + archivage long terme hors stack.

## Rétentions

| Système | Rétention "chaude" | Cap taille | Archivage (prod) |
|---------|--------------------|------------|------------------|
| Prometheus | 30 j | 10 Go | remote_write → Mimir / VictoriaMetrics / Thanos |
| Loki | 30 j (720 h) | illimité local (filesystem) | filesystem dev / S3 prod via `object_store: s3` |
| Temporal history | 90 j par défaut | n/a | Postgres Temporal (isolé) |
| Grafana dashboards | ∞ | n/a | provisionnés depuis git, pas de mutation runtime |

## Dev stack

- Prometheus : `--storage.tsdb.retention.time=30d --storage.tsdb.retention.size=10GB` + volume `prometheusdata`.
- Loki : config [docker/loki/loki-config.yml](../../docker/loki/loki-config.yml) avec `retention_period: 720h` + compactor actif + volume `lokidata`.

Vérifier l'usage disque à tout moment :

```sh
docker exec -it kubemanager-prometheus-1 du -sh /prometheus
docker exec -it kubemanager-loki-1       du -sh /loki
```

## Prod — remote_write / object store

**Prometheus — remote_write vers Mimir/VM/Thanos** :

```yaml
remote_write:
  - url: https://mimir.internal/api/v1/push
    basic_auth:
      username: kubemanager
      password_file: /etc/secrets/mimir-password
    write_relabel_configs:
      # Drop les metrics de debug Go verbeuses — elles saturent Mimir pour pas grand-chose
      - source_labels: [__name__]
        regex: 'go_gc_.*|go_memstats_.*'
        action: drop
```

**Loki — S3 comme object_store** :

```yaml
storage_config:
  aws:
    s3: s3://ACCESS:SECRET@us-east-1/kubemanager-loki-prod
    s3forcepathstyle: true
schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb
      object_store: s3
      schema: v13
      index:
        prefix: index_
        period: 24h
```

Avec S3 : rétention infinie peu coûteuse, suppression pilotée par `retention_period` du compactor.

## Nettoyage d'urgence (disque plein)

1. **Prometheus** : appeler `POST /api/v1/admin/tsdb/delete_series?match[]={job="kubemanager"}` puis `POST /-/reload`. Ou simplement `docker volume rm prometheusdata` (perte des métriques chaudes).
2. **Loki** : utiliser l'API `POST /loki/api/v1/delete` avec `query` + `start`/`end` ; le compactor purge au prochain cycle (10 min).

## Cardinalité — guardrails

- `gin_requests_total` : borné à ~50 séries par replica (routes × codes × méthodes). ✓
- `temporal_workflow_*` : borné à (workflow_type × result × task_queue). ✓
- Business metrics custom : `kubemanager_cluster_last_seen_timestamp_seconds` a `cluster_id` en label → surveiller si > 1k clusters, envisager top-N seulement.
- **Règle** : toute nouvelle métrique avec un label "user_id", "workload_id", "request_id" est bannie — utiliser les traces OTEL pour ces dimensions.

## Audit périodique

- **Chaque fin de trimestre** : dump des séries `prometheus_tsdb_head_series` par `__name__` (`top 50`), identifier les dérives, ajouter des `metric_relabel_configs` drop si nécessaire.
- **Alert** : `prometheus_tsdb_head_series > 1_000_000` → review required.
