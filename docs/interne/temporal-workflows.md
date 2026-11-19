# Temporal workflows & saga

> Tous les effets externes (K8s, Helm, Keycloak) passent par Temporal pour garantir retry + compensation.

## Patterns

- **Saga** : chaque workflow critique (`create project`, `delete workload`) déclare une série d'activités + leurs compensations.
- **Idempotence** : `idempotency.ExecuteActivity()` wraps l'activité avec une clé = `workflowRunID + activityName + inputHash`. TTL 10 min. Évite les effets doubles sur retry.
- **TaskDistributor** : wrapper autour du Temporal client pour abstraire la création de workflows depuis les services.

## Workflows existants

- `projects/workflows/create.go` — create project saga.
- `workloads/workflows/deleteWorkload.go` — delete workload saga.

## Retry policy (défaut)

```go
RetryPolicy{
  InitialInterval:    time.Second,
  MaximumAttempts:    3,
  MaximumInterval:    time.Minute,
}
```

> **Stub** — ajouter un template "comment créer un nouveau workflow" + checklist compensation.
