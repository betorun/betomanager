# KubeManager — Vue d'ensemble technique

> Plateforme CaaS multi-tenant (type Heroku/Scaleway) orchestrée par Temporal, déployant des workloads Helm sur clusters K3s/K8s distants via un watcher.

## Stack

| Couche | Technologie |
|--------|-------------|
| Langage | Go 1.25 |
| HTTP | Gin + Fizz (OpenAPI auto-généré) |
| ORM | GORM + PostgreSQL |
| Cache | Redis (avec fallback in-memory) |
| Workflow engine | Temporal (saga + activités idempotentes) |
| Deploy engine | Helm SDK v3 + chart `standard-app` (PSA restricted) |
| Auth | Keycloak (OIDC, ROPC, social) + JWT HS256 |
| Realtime | Centrifugo v5 (WebSocket broker, HS256 tokens) |
| Cluster watcher | K8s client-go (pods/events/leases) |

## Modules

| Module | Rôle principal |
|--------|----------------|
| `auth` | Users, rôles, RBAC projet, API keys, sessions, JWT |
| `identity` | Bootstrap Keycloak (register/login/refresh), social OAuth |
| `projects` | Provisioning namespace K8s + saga Temporal (secrets, quotas, RBAC) |
| `workloads` | Lifecycle workload : install Helm, scale, restart, rollback, delete saga |
| `clusters` | Watcher enrollment, config dispatch, pod events ingestion |
| `realtime` | Centrifugo tokens (connection + subscription) + Publisher |
| `apikeys` | Clés user + clés cluster, rotation, audit, scopes |
| `admin` | Endpoints admin (cluster-keys, liste clusters, monitoring) |

## Structure du dépôt

```
cmd/
  server/              → entrypoint HTTP + worker Temporal
internal/
  core/
    cache/             → Redis + fallback
    configs/           → env vars, feature flags
  infrastructure/
    router/            → DI + registration Fizz
    helm/              → chart standard-app + SDK
    realtime/          → Publisher + TokenSigner Centrifugo
    idempotence/       → Begin/Complete/Fail pattern
    database/migrations/
  middleware/          → auth JWT, api key, tenant, rate limit, audit
  modules/
    {auth,identity,projects,workloads,clusters,realtime}/
      domain/          → DTOs + entities GORM
      repository/      → accès DB
      service/         → logique métier
      activities/      → Temporal activities
      workflows/       → Temporal workflows (saga)
docs/
  devportal.html       → ce portail
  devportal-data/      → chunks markdown lazy-loadés
```

## Patterns architecturaux

- **Saga Temporal** : chaque workflow critique (create project, delete workload) déclare ses compensations pour garantir l'atomicité cross-système (DB + K8s + Keycloak).
- **Idempotence** : les activités Helm/K8s utilisent `idempotency.ExecuteActivity()` (TTL 10 min) pour éviter les doubles effets sur retry Temporal.
- **Isolation tenant** : toute query filtre par `project_id`, les namespaces K8s sont préfixés `km-<project-id>`.
- **API keys duales** : `kind=user` (scopes) pour les devs, `kind=cluster` pour les watchers (rotation + grace period).
- **Publisher fail-open** : publication Centrifugo best-effort, erreur loggée mais pas remontée (pas de bloquage du flux métier).
- **Watcher Restate-compatible** : le watcher K8s appelle Enroll → GetConfig → OnPodEvent. Persistance en table `clusters` avec `last_seen_at` pour détection staleness.

## Guides internes

- [Architecture technique](#guide:interne-architecture)
- [Temporal workflows & saga](#guide:interne-temporal-workflows)
- [Helm chart hardening (PSA)](#guide:interne-helm-hardening)
- [Keycloak bootstrap & dev stack](#guide:interne-keycloak-bootstrap)
- [Centrifugo realtime E2E](#guide:interne-centrifugo-realtime)
- [Middleware chain HTTP](#guide:interne-middleware-chain)
- [Dev stack locale (make dev-up)](#guide:interne-dev-stack)
