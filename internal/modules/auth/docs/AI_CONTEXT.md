# `auth` — AI Context

## Invariants
- `userID` toujours issu du JWT (middleware), jamais du payload client.
- Chaque API key a un `kind` (user|cluster), un `prefix` visible et un `hash` BCrypt.
- `CheckProjectPermission(userID, projectID, slug)` centralise tous les checks RBAC.

## Règles
- Ne pas exposer le hash ni le token plein (seul le prefix + token clair au create).
- Le `RevokeAllUserSessions` est le hard-logout (force rotation).
