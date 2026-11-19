# Module `workloads`

> Lifecycle complet d'un workload : deploy (Helm), scale, restart, rollback, logs, events, delete (saga Temporal).

## Endpoints HTTP

Exposés sous `/projects/:projectID/workloads` — protégés par `RequireProjectMembership`.

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST`   | `/workloads` | Créer + déployer un workload (Helm install) |
| `GET`    | `/workloads` | Lister les workloads du projet |
| `GET`    | `/workloads/:id` | Détails (état DB) |
| `PATCH`  | `/workloads/:id` | Mettre à jour image / env / storage (Helm upgrade) |
| `DELETE` | `/workloads/:id` | Supprimer — saga Temporal (Helm uninstall + soft delete DB) |
| `POST`   | `/workloads/:id/stop` | Scale to 0 |
| `POST`   | `/workloads/:id/start` | Scale back to N |
| `POST`   | `/workloads/:id/restart` | Rollout restart (`kubectl rollout restart`) |
| `POST`   | `/workloads/:id/rollback` | Rollback Helm à la révision N-1 |
| `GET`    | `/workloads/:id/status` | Statut live (DB + runtime K8s) |
| `GET`    | `/workloads/:id/events` | Events K8s du workload |
| `GET`    | `/workloads/:id/logs` | Stream logs pod (chunked HTTP) |

Routes déclarées dans `internal/infrastructure/router/route.go:136-158`.

## Saga Temporal — Delete

Fichier : `internal/modules/workloads/workflows/deleteWorkload.go`.

Design **"Helm first, DB second"** — minimise les orphelins K8s si la DB échoue.

```
┌─────────────────────────┐
│ 1. UninstallChart       │  retry 3x, timeout 5min, nil si release absente
└───────────┬─────────────┘
            │ ok
            ▼
┌─────────────────────────┐
│ 2. SoftDeleteWorkload   │  UPDATE workloads SET deleted_at=now()
└───────────┬─────────────┘
            │ ok
            ▼
      broadcast DELETED
     (Centrifugo best-effort)
```

En cas d'échec étape 1 → `UpdateWorkloadStatus(DELETE_FAILED)` + publish Centrifugo. Pas de compensation auto : l'admin retry via l'endpoint dédié.

Activités : `internal/modules/workloads/activities/heml.go` (UninstallChart, InstallChart) et `activities/db.go` (UpdateWorkloadStatus, SoftDeleteWorkload).

## Install / Upgrade Helm

`InstallChart` dans `activities/heml.go:77` :

- Charge le chart `/app/internal/infrastructure/helm/charts/standard-app`.
- `helm upgrade --install`, `wait=true`, timeout 5 min.
- Idempotence via `idempotency.ExecuteActivity()` (TTL 10 min sur le contexte activity).

Inputs injectés dans les `values.yaml` :

```yaml
image:
  repository: {{ ImageRepo }}
  tag:        {{ ImageTag }}
replicaCount: {{ Replicas }}
resources:
  requests: { cpu: {{ CPURequest }}, memory: {{ MemoryRequest }} }
  limits:   { memory: {{ MemoryLimit }} }
persistence:
  enabled:      {{ PersistenceEnabled }}
  size:         {{ StorageSize }}
  storageClass: {{ StorageClass }}
tls:
  enabled: {{ TLSEnabled }}
healthcheck:
  path: {{ HealthPath }}
autoscaling:
  enabled: {{ AutoscalingEnabled }}
pdb:
  enabled: {{ PDBEnabled }}
securityContext:
  runAsUser:  {{ RunAsUser }}
  runAsGroup: {{ RunAsGroup }}
```

Le chart standard-app est **PSA restricted-compliant** (voir guide `interne-helm-hardening`).

## Événements Centrifugo

`UpdateWorkloadStatus` (db.go:44) persiste d'abord en DB (source de vérité), puis publie :

- Channel : `project:<projectID>:workload:<workloadID>:deploy`
- Payload : `DeploymentEvent{workload_id, project_id, status, phase, at}`
- Best-effort : erreur loggée, pas remontée.

## Dépendances

- Helm SDK : `internal/infrastructure/helm/helm.go`.
- Temporal client : injecté via `TaskDistributor`.
- Publisher realtime : `internal/infrastructure/realtime/publish.go`.
- K8s client : chargé depuis kubeconfig du cluster cible (résolu via `projects.Cluster`).
