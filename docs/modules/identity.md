# Module `identity`

> Bootstrap Keycloak : register, login (ROPC), refresh tokens, social OAuth (Google/GitHub), reset password. Le module qui parle à Keycloak — `auth` ne s'en occupe pas.

## Responsabilités

- Création d'un user Keycloak + sync de la row locale `users`.
- Login ROPC → JWT kubemanager.
- Refresh token rotation (stocké en DB, audit `last_used_at`).
- Social OAuth : génération URL `/social/:provider`, callback `/social/callback`.
- Change password, reset password, resend verification email.
- Bootstrap local : premier projet, assignation du rôle owner.
- Usage de l'Admin REST API Keycloak (groupes, users, reset).

## Endpoints HTTP

Tous sous `/kmanager/v1/identity`. Voir [route.go:66-88](../../internal/infrastructure/router/route.go#L66).

### Public (pas d'auth requise)

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/register` | Crée un user Keycloak + envoie l'email de verification |
| `POST` | `/login` | ROPC : email/password → JWT + refresh |
| `POST` | `/refresh` | Renouvelle tokens via refresh_token |
| `POST` | `/forgot-password` | Envoie email de reset |
| `POST` | `/resend-verification` | Renvoie email de verification |
| `GET` | `/social/:provider` | URL d'autorisation (Google/GitHub) |
| `GET` | `/social/callback` | Callback OAuth2 |

### Authentifié

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/logout` | Révoque refresh_token + blacklist JTI |
| `GET` | `/validate` | Valide le token courant |
| `POST` | `/change-password` | Change le password (nécessite l'ancien) |
| `POST` | `/bootstrap` | Provisionne profil local + projet initial |
| `GET` | `/bootstrap/status` | Vérifie si le bootstrap est fait |

## Tables DB

Aucune spécifique — utilise `users`, `user_sessions`, `revoked_tokens` du module `auth`.

## Activities Temporal

Aucune. Tous les appels à Keycloak sont synchrones dans le service.

## Invariants critiques

1. **Keycloak est la source de vérité** pour l'authentification. Si Keycloak dit "invalide", kubemanager doit refuser.
2. **La table `users` miroir Keycloak** via la clé de jointure `keycloak_user_id`. Toujours `upsert` via `FindByKeycloakUserID` avant de créer un user local.
3. **Les groupes Keycloak reflètent les projets** (`project-<uuid>`) — simplifie l'UI admin Keycloak, et permet au JWT de porter les memberships.
4. **ROPC est DEV-ONLY** — en prod, le client `kubemanager-api` doit avoir `directAccessGrants: false`. Seul `kubemanager-dev-cli` autorise ROPC (pour la CI et les scripts de test).

## Pitfalls

- **Race condition register** — si deux register concurrents pour le même email arrivent, seule la première réussit côté Keycloak. Le second reçoit une erreur 409 Keycloak → mapper en `CodeConflict`.
- **Clock skew** — le JWT Keycloak a un `nbf` ; si la machine host a une heure décalée, les tokens sont rejetés. Toujours NTP sync.
- **Email verified** — un user peut login sans avoir vérifié son email si `emailVerified: false` côté Keycloak. Pour bloquer : ajouter un check dans l'AuthMiddleware ou un required action côté Keycloak.

## Tests

- Pas de tests unitaires dédiés (le module est principalement un adapter vers Keycloak — l'utilisation est testée via les scripts e2e).
- Tests e2e : [scripts/e2e.sh](../../scripts/e2e.sh) + [scripts/keycloak/get_test_token.sh](../../scripts/keycloak/get_test_token.sh).

## Fichiers clés

- [internal/modules/identity/](../../internal/modules/identity/) — service + controller.
- [internal/infrastructure/keycloak/](../../internal/infrastructure/keycloak/) — client Keycloak (admin REST + token endpoint).
- Pour tester localement : [scripts/keycloak/bootstrap_platform.sh](../../scripts/keycloak/bootstrap_platform.sh) crée le realm + client + users.

## Points d'attention

- Les secrets Keycloak admin sont chargés depuis `config.Keycloak.AdminClientSecret` (env `KUBEMANAGER_KEYCLOAK_ADMIN_CLIENT_SECRET`).
- Pour ajouter un IdP social : configurer le client côté Keycloak admin console, puis adapter `SocialLoginURL` / `SocialCallback`.
- Pour désactiver ROPC en prod : ne pas setter `kubemanager-dev-cli` dans le bootstrap prod (voir [interne/keycloak-bootstrap.md](../interne/keycloak-bootstrap.md)).
