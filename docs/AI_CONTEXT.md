# KubeManager — AI Context global

> Source autoritaire pour systèmes RAG/assistants qui raisonnent sur kubemanager. Pour les contextes par module : voir les modules individuels.

## Identité du projet

**KubeManager** est une plateforme CaaS (Container-as-a-Service) multi-tenant écrite en Go. Elle permet à des développeurs de déployer des applications containerisées sur des clusters Kubernetes (K3s ou managed K8s) via une API HTTP unifiée. Concurrente de Heroku, Scaleway Container Service, Fly.io.

## Architecture en une phrase

HTTP Gin+Fizz → Temporal workflows (saga) → Helm activities → K3s/K8s distant ; watcher K8s pousse pod events vers kubemanager ; Centrifugo broadcast aux frontends.

## Modules et responsabilités

| Module | Responsabilité |
|--------|----------------|
| `auth` | Users, sessions, API keys (user + cluster), RBAC projet, scopes |
| `identity` | Bootstrap Keycloak : register/login/refresh/social OAuth |
| `projects` | Namespace K8s + saga provisioning (Keycloak group, secrets, quotas, RBAC, ClusterRoleBinding) |
| `workloads` | 12 endpoints lifecycle ; saga Delete (Helm uninstall + soft delete DB) ; start/stop/restart/rollback/status/events/logs |
| `clusters` | Enroll watcher, GetConfig, OnPodEvent ; admin ListClusters |
| `realtime` | /realtime/token + /realtime/subscribe ; Centrifugo HS256 ; Publisher HTTP API |
| `apikeys` | ListKeys/GetKey/ListAudit/CreateKey/RevokeKey + Cluster keys (Create/List/Rotate) |
| `admin` | Endpoints admin protégés RequireGlobalRole(SUPER_ADMIN/ADMIN) |

## Règles transverses critiques

1. **Isolation tenant** : toute query DB filtre par `project_id`. Jamais de données cross-project.
2. **User ID du JWT** : `userID` extrait du context middleware — jamais du payload client.
3. **Idempotence activités** : toute activité Temporal qui touche K8s/Helm passe par `ExecuteActivity()` (TTL 10 min).
4. **Namespace K8s** : préfixé `km-<project-id>` pour isolation.
5. **Helm first, DB second** (delete) : minimise les orphelins K8s en cas de panne partielle.
6. **Publisher fail-open** : Centrifugo non critique, erreur non remontée.
7. **API key cluster** : kind="cluster", scope `cluster:report`, utilisée par le watcher via X-API-Key.

## Stack détaillée

- **Go 1.25** — type-safety + goroutines pour le worker pool.
- **Gin + Fizz** — OpenAPI auto-généré depuis les signatures controller.
- **GORM + PostgreSQL** — users, projects, workloads, api_keys, clusters, sessions.
- **Redis** — cache + rate limit ; fallback in-memory.
- **Temporal** — workflows + activities ; saga pattern ; retry policies.
- **Helm SDK v3** — install/upgrade/rollback/uninstall depuis le chart standard-app.
- **Keycloak** — OIDC/JWT, groupes projet, admin REST API.
- **Centrifugo v5** — WebSocket broker, tokens HS256.
- **client-go** — côté watcher : informers pods/events, leader election lease.

## Tables principales

- `users` / `user_sessions` / `user_roles`
- `api_keys` (kind: user|cluster, scopes, last_used_at, revoked_at)
- `projects` (owner_user_id, namespace, status)
- `project_members` (role: owner|admin|developer|viewer, status: ACTIVE|INVITED|APPROVED)
- `workloads` (project_id, name, status, helm_revision, image, replicas, resources)
- `clusters` (cluster_id, version, last_seen_at, enrolled_at)

## Références

- Architecture : voir guide `interne-architecture`.
- Saga Temporal : voir guide `interne-temporal-workflows`.
- Helm hardening : voir guide `interne-helm-hardening`.
- Middleware chain : voir guide `interne-middleware-chain`.
