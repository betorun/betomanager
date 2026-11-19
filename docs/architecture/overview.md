# Architecture — vue d'ensemble

> Une page, trois diagrammes, trois niveaux de zoom.

## C4 niveau 1 — Contexte

Qui parle à KubeManager et à quoi il parle.

```mermaid
graph LR
    Dev[Développeur / CI]
    Admin[Admin plateforme]
    Frontend[Dashboard frontend]
    KM[KubeManager]
    KC[Keycloak]
    PG[(PostgreSQL)]
    Redis[(Redis)]
    Cluster1[Cluster K3s / K8s]
    Cluster2[Cluster K3s / K8s]
    Centri[Centrifugo]

    Dev -->|REST API| KM
    Admin -->|REST API admin| KM
    Frontend -->|REST API + WS| KM
    KM -->|OIDC / JWT| KC
    KM --> PG
    KM --> Redis
    KM -->|Helm| Cluster1
    KM -->|Helm| Cluster2
    Cluster1 -->|Pod events via watcher| KM
    Cluster2 -->|Pod events via watcher| KM
    KM -->|HTTP publish| Centri
    Frontend -->|WebSocket| Centri
```

**Responsabilité de kubemanager** : être le plan de contrôle. Piloter N clusters K8s distants, exposer une API unifiée pour les devs, broadcaster les events live aux frontends.

## C4 niveau 2 — Containers

Les processus qui tournent dans un déploiement kubemanager (le binaire + ses dépendances).

```mermaid
graph TB
    subgraph Binaire["kubemanager (Go)"]
        Server[HTTP Server<br/>APP_MODE=server]
        Worker[Temporal Worker<br/>APP_MODE=worker]
    end

    subgraph Infra["Infra partagée"]
        PG[(PostgreSQL<br/>11 tables)]
        Redis[(Redis<br/>cache + rate limit + idempotence)]
        Temp[(Temporal<br/>workflow engine)]
        Centri[Centrifugo v5<br/>WebSocket broker]
    end

    subgraph Obs["Observabilité"]
        Prom[Prometheus]
        Loki[Loki]
        Jaeger[Jaeger]
    end

    Server --> PG
    Server --> Redis
    Server --> Temp
    Server --> Centri
    Worker --> Temp
    Worker --> PG
    Worker -->|Helm / K8s| Clusters[Clusters gérés]

    Server -. /metrics, logs stdout, OTLP .-> Prom
    Server -. /metrics, logs stdout, OTLP .-> Loki
    Server -. /metrics, logs stdout, OTLP .-> Jaeger
    Worker -. metrics Temporal .-> Prom
```

**Un seul binaire**, deux modes sélectionnés par `APP_MODE` :
- `server` — expose l'API HTTP, démarre pas de worker Temporal.
- `worker` — consomme la task queue Temporal, exécute les activities.

Permet de scaler indépendamment (HPA sur le deploy server, nombre fixe de workers).

## C4 niveau 3 — Composants (mode server)

Les paquets Go dans le binaire quand il tourne en `APP_MODE=server`.

```mermaid
graph LR
    subgraph Transport["Transport (HTTP)"]
        Gin[Gin + Fizz]
        MW[Middlewares<br/>auth, rate-limit, idempotence, trace]
    end

    subgraph Modules["Domaine métier"]
        Auth[auth<br/>users, API keys]
        Id[identity<br/>Keycloak]
        Proj[projects<br/>namespace + saga]
        Work[workloads<br/>lifecycle]
        Cls[clusters<br/>watcher protocol]
        Rt[realtime<br/>Centrifugo tokens]
    end

    subgraph Infra["Infrastructure"]
        DB[(GORM + otelgorm)]
        RedisC[Redis client]
        TDist[TaskDistributor]
        KCC[Keycloak client]
        Pub[Centrifugo Publisher]
        TSig[TokenSigner HMAC]
    end

    Gin --> MW
    MW --> Modules
    Auth --> DB
    Id --> KCC
    Proj --> DB
    Proj --> TDist
    Work --> DB
    Work --> TDist
    Cls --> DB
    Cls --> Pub
    Rt --> TSig
    MW --> RedisC
```

**Règles d'import** :
- Un module métier peut importer un `infrastructure/*` → ✅.
- Un module métier peut importer les `repository/` d'un autre module → ✅ (cross-module read).
- Un module métier peut importer `service/` d'un autre module → ❌ refusé en review (couplage fort).
- `infrastructure/*` ne peut pas importer `modules/*` → ✅ (direction unique).

## Données — 11 tables, 4 zones

```mermaid
erDiagram
    users ||--o{ user_sessions : "a N sessions"
    users ||--o{ api_keys : "possède N clés"
    users }o--o{ projects : "via project_members"
    projects ||--o{ project_members : "a N membres"
    projects ||--o{ workloads : "a N workloads"
    roles ||--o{ role_permissions : ""
    permissions ||--o{ role_permissions : ""
    roles }o--o{ project_members : "rôle projet"
```

Zones :

| Zone | Tables |
|------|--------|
| Auth & sessions | `users`, `user_sessions`, `revoked_tokens` |
| RBAC | `roles`, `permissions`, `role_permissions`, `project_members` |
| Ressources | `projects`, `workloads`, `api_keys` |
| Fleet | `clusters` |

Schéma complet : [migrations/000001_init.up.sql](../../migrations/000001_init.up.sql).

## Choix structurants

Pour ne pas répéter les "pourquoi" ici, voir les ADRs :

- [ADR-0001 — Temporal plutôt qu'Asynq](../adr/0001-temporal-over-asynq.md)
- [ADR-0002 — Helm SDK v3 intégré plutôt que shell](../adr/0002-helm-sdk-v3.md)
- [ADR-0003 — Centrifugo fail-open](../adr/0003-centrifugo-fail-open.md)
- [ADR-0004 — Cluster watcher comme binaire séparé](../adr/0004-cluster-watcher-separate.md)

## Prochaine lecture

- [Cycle de vie d'une requête](request-lifecycle.md) — le parcours `POST /workloads` de bout en bout.
- [Sagas Temporal](temporal-sagas.md) — compensation, idempotence, retry policy.
- [Sécurité](security.md) — auth chain, RBAC, NetworkPolicy.
