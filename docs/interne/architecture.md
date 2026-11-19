# Architecture technique

> Guide de référence pour comprendre la structure du dépôt, les frontières entre modules et les patterns transverses.

## Couches

1. **Transport** (`internal/infrastructure/router/`) — Fizz/Gin, DI, registration routes.
2. **Middleware** (`internal/middleware/`) — auth JWT, API key, tenant resolver, rate limit, audit, idempotence.
3. **Modules métier** (`internal/modules/<name>/`) — controller → service → repository + activities/workflows Temporal.
4. **Infrastructure** (`internal/infrastructure/`) — Helm SDK, Keycloak client, realtime (Centrifugo), database, idempotence.
5. **Core** (`internal/core/`) — config, cache.

## DI

`router/dependencies.go` expose un `App` qui porte tous les singletons (DB, Redis, Temporal client, Keycloak, Helm runner, Publisher, TokenSigner). Injecté dans les controllers via constructeurs.

## Conventions

- Un dossier par module, un fichier `h<module>.go` pour le controller entry-point.
- Les DTOs dans `domain/`, les entities GORM également (tag `gorm:`).
- Les workflows Temporal dans `workflows/`, les activities dans `activities/`.

> **Stub** — compléter avec diagrammes (C4) + trace d'une requête typique.
