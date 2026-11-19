# Ajouter une métrique Prometheus

> Les règles de cardinalité chez nous sont strictes. Lire [observability](../architecture/observability.md) d'abord.

## Checklist (5 s)

- [ ] Nom commence par `kubemanager_` (business) ou est sur les préfixes standards (`gin_`, `go_`, `temporal_`, `db_sql_`, `redis_`).
- [ ] Labels bornés — pas de `user_id`, `workload_id`, `project_id`, `request_id`.
- [ ] Type choisi : Counter, Gauge, Histogram — **jamais Summary** (pas scalable).
- [ ] Documenté dans [business_metrics.go](../../internal/infrastructure/observability/business_metrics.go).

## Où placer le code

Toutes les métriques custom vivent dans [internal/infrastructure/observability/business_metrics.go](../../internal/infrastructure/observability/business_metrics.go).

**Ne crée pas** un `metrics.go` par module — centraliser évite les doublons, facilite l'audit cardinalité.

## Exemple — counter simple

```go
// WorkloadScalesTotal : total des scale up/down déclenchés.
// Nourrit le panel "scaling activity" du dashboard Grafana.
var WorkloadScalesTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "kubemanager",
        Subsystem: "workload",
        Name:      "scales_total",
        Help:      "Total number of scale operations on workloads.",
    },
    []string{"direction"}, // "up" | "down" — 2 valeurs max, safe
)
```

Usage dans le service :

```go
observability.WorkloadScalesTotal.WithLabelValues("up").Inc()
```

## Exemple — gauge avec timestamp

```go
var ClusterLastSeenTimestamp = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace: "kubemanager",
        Subsystem: "cluster",
        Name:      "last_seen_timestamp_seconds",
        Help:      "Unix timestamp of the last watcher heartbeat.",
    },
    []string{"cluster_id"},
)
```

Usage :

```go
observability.ClusterLastSeenTimestamp.WithLabelValues(clusterID).SetToCurrentTime()
```

## Exemple — histogram de durée

```go
var HelmInstallDuration = promauto.NewHistogramVec(
    prometheus.HistogramOpts{
        Namespace: "kubemanager",
        Subsystem: "helm",
        Name:      "install_duration_seconds",
        Help:      "Duration of helm install/upgrade calls.",
        Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5 s → 256 s
    },
    []string{"outcome"}, // "success" | "failed"
)
```

Usage :

```go
start := time.Now()
err := helmClient.Install(...)
outcome := "success"
if err != nil { outcome = "failed" }
observability.HelmInstallDuration.
    WithLabelValues(outcome).
    Observe(time.Since(start).Seconds())
```

**Choix des buckets** :
- Pour de la latence ms → `{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}` (défaut gin).
- Pour des opérations longues (seconds) → `ExponentialBuckets(0.5, 2, 10)` = `{0.5, 1, 2, 4, 8, 16, 32, 64, 128, 256}`.
- Aligner sur les SLOs — si ton SLO P95 est 1 s, faut un bucket à 1 s pour que `histogram_quantile` soit précis autour.

## Cardinalité — bannir absolument

❌ `user_id`, `workload_id`, `project_id`, `request_id`, `trace_id`, `pod_name` — cardinalité unbounded → Prometheus OOM.

❌ `url_path` raw — utiliser le route template (`/workloads/:id` plutôt que `/workloads/abc-def-uuid`).

✅ `status`, `phase`, `outcome`, `method`, `code` — enum finis.

✅ `cluster_id` — borné (< 1000 attendu), OK.

✅ `handler` (route template) — notre cardinalité tient.

Ce qui belong dans les **traces OTEL** : user_id, workload_id, request_id. Ces dimensions sont exploitables via Jaeger, pas via Prometheus.

## Piège fréquent — double registration

`promauto` enregistre dans `prometheus.DefaultRegisterer` au package init. Si tu déclares deux `NewCounterVec` avec le même nom, tu crashes au boot avec une panic "duplicate metric".

→ Si tu veux des métriques de test, utiliser un `prometheus.NewRegistry()` local.

## Dashboard Grafana

Pour que ta métrique apparaisse dans un dashboard :

1. Vérifier qu'elle est scrape-able — `curl http://localhost:9100/metrics | grep kubemanager_workload_scales_total`.
2. Éditer un dashboard existant dans `docker/grafana/provisioning/dashboards/*.json` ou en créer un nouveau.
3. Commit le JSON — Grafana les recharge auto (interval 30 s).

## Alerte PrometheusRule

Si la métrique doit déclencher une alerte :

1. Ajouter une rule dans [chart/kubemanager-backend/templates/prometheusrule.yaml](../../chart/kubemanager-backend/templates/prometheusrule.yaml).
2. `for:` au moins 5 min pour éviter les alertes flaky.
3. Une `runbook_url` qui pointe vers [interne/runbook.md](../interne/runbook.md).

## Tests

Pas de tests unitaires Prometheus (les métriques sont des globals stateful). Par contre, vérifier que les appels sont bien faits dans les tests du service ou de l'activity :

```go
// Avant l'appel
v0 := testutil.ToFloat64(WorkloadScalesTotal.WithLabelValues("up"))

svc.ScaleUp(...)

// Après
require.Equal(t, v0+1, testutil.ToFloat64(WorkloadScalesTotal.WithLabelValues("up")))
```

Import : `github.com/prometheus/client_golang/prometheus/testutil`.

## Post-merge

- Vérifier en dev : Grafana → Explore → `kubemanager_workload_scales_total` → série visible.
- Vérifier la cardinalité : `prometheus_tsdb_head_series` reste stable sur le dashboard infra.
