# Module `projects`

> Provisioning d'un projet = namespace K8s isolé + saga Temporal (Keycloak group, secrets, quotas, RBAC).

## Saga create project

1. `CreateProjectRow` (DB)
2. `CreateKeycloakGroup` (groupe `project-<id>`)
3. `AssignProjectRole` (lie owner au groupe)
4. `CreateNamespace` (K8s, label `km-project-id`)
5. `EnsureSecret` (image pull secret + registry creds)
6. `ApplyResourceQuota` (limites CPU/mem/pods)
7. `ApplyClusterRoleBinding` (SA → role developer)

Chaque étape a sa compensation (idempotente).

## Fichiers
- `internal/modules/projects/project.go` — entrypoint module
- `internal/modules/projects/workflows/create.go` — saga
- `internal/modules/projects/activities/k8s.go` — activités K8s
- `internal/modules/projects/service/create.go` — orchestration

> **Stub** — à enrichir (RBAC mapping, quotas par plan, archivage).
