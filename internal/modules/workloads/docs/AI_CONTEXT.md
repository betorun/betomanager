# `workloads` — AI Context

## Responsabilité
Lifecycle d'une application déployée sur Kubernetes via Helm : create (install), update (upgrade), scale, restart, rollback, logs, events, delete (saga).

## Invariants critiques
- **Saga Delete** : Helm uninstall toujours avant soft delete DB.
- **Idempotence** : toute activité Temporal passe par `ExecuteActivity()` (idempotency key = workflow run id + activity name).
- **Isolation** : les namespaces K8s sont `km-<project-id>`, jamais cross-project.
- **Retry** : UninstallChart max 3x, timeout 5 min ; nil si release absente (safe retry).
- **Publisher** : fail-open sur Centrifugo — l'échec du broadcast ne casse jamais la transaction métier.

## Endpoints (12)
- CRUD : POST `/workloads`, GET list, GET one, PATCH, DELETE
- Lifecycle : POST `/start`, `/stop`, `/restart`, `/rollback`
- Observability : GET `/status`, `/events`, `/logs`

Tous sous `/projects/:projectID/workloads`. Guard `RequireProjectMembership`.

## Channels Centrifugo
- `project:X:events` — dashboard projet
- `project:X:workload:Y` — détails workload
- `project:X:workload:Y:deploy` — transitions de statut + pod events du watcher

## Status DB
- `PENDING` → `DEPLOYING` → `RUNNING` / `FAILED`
- `SCALING_UP` / `SCALING_DOWN`
- `RESTARTING` / `UPDATING` / `ROLLING_BACK`
- `DELETING` → `DELETED` / `DELETE_FAILED`

## Fichiers clés
- `internal/modules/workloads/hworkloads.go` — controller
- `internal/modules/workloads/service/*.go` — logique (createWorkload, deleteWorkload, lifecycle…)
- `internal/modules/workloads/workflows/deleteWorkload.go` — saga
- `internal/modules/workloads/activities/heml.go` — Install/UninstallChart
- `internal/modules/workloads/activities/db.go` — persistence + publish
- `internal/infrastructure/helm/charts/standard-app/` — chart PSA restricted

## Règles pour l'assistant
- Ne jamais supprimer directement en DB sans passer par la saga.
- Toujours valider les inputs resources (CPU/mem) côté service avant Helm.
- Les secrets workload sont injectés par `projects.EnsureSecret` — pas de manipulation directe ici.
