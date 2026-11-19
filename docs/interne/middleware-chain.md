# Middleware chain HTTP

> Ordre strict des middlewares Gin (important : l'ordre conditionne l'accès au context).

## Ordre

1. `headers` — trace ID, request ID, CORS preflight.
2. `rate_limit` — token bucket Redis, fallback in-memory.
3. `audit` — log structuré (avant auth pour capturer aussi les 401).
4. `auth` (JWT Bearer) OU `apikey_auth` (X-API-Key) — pose `userID` / `apiKeyID` dans le context.
5. `tenant_resolver` — résout `project_id` depuis path ou header, pose `projectID`.
6. `idempotence` — Begin/Complete/Fail pour les POST/DELETE critiques (header `Idempotency-Key`).

## Fichiers

- `internal/middleware/middleware.go` — assembly
- `internal/middleware/security/middleware.go` — JWT auth
- `internal/middleware/apikey_auth.go`
- `internal/middleware/tenant_resolver.go`
- `internal/middleware/rateLimit.go`
- `internal/middleware/idempotence.go`
- `internal/middleware/audit.go`
- `internal/middleware/headers.go`

> **Stub** — ajouter diagramme + matrice route/middlewares.
