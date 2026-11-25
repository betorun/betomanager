# Module `identity`

> Authentification des personnes, gérée par betorun avec Keycloak comme annuaire privé ([auth.md](../architecture/auth.md), « Keycloak invisible ») : inscription, connexion, sessions et jetons betorun, second facteur, emails, Google/GitHub, connexion de la CLI. Le module qui parle à Keycloak — `auth` ne s'en occupe pas.

## Responsabilités

- Inscription : compte créé dans Keycloak + row locale `users`, email de vérification envoyé **par betorun** (SMTP, modèles FR/EN).
- Connexion par mot de passe : vérifié auprès de Keycloak par un client confidentiel réservé au backend (les jetons Keycloak sont jetés), puis **session betorun** (`auth_sessions`) et jeton d'accès betorun.
- Jeton de rafraîchissement opaque, haché, tournant à chaque usage (réutilisation = session révoquée).
- Second facteur : TOTP, codes de secours, step-up (`/session/verify`).
- Sessions : liste, fermeture d'une session ou de toutes les autres.
- Vérification d'email, mot de passe oublié, changement de mot de passe (jetons à usage unique, liens vers des pages Next.js).
- Google / GitHub en direct (state, PKCE, nonce gardés côté serveur), comptes liés.
- CLI : autorisation depuis la console (PKCE, retour `127.0.0.1`) ou code d'appareil (RFC 8628) géré par betorun.
- Bootstrap local : profil, premier projet, rôle owner.

## Endpoints HTTP

Tous sous `/kmanager/v1/identity`. Déclarés dans `addIdentityRoutes` et `addAuthRoutes` de [route.go](../../internal/infrastructure/router/route.go).

### Public (pas d'auth requise)

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/register` | Crée un compte + email de vérification (réponse identique si l'adresse a déjà un compte). `country` facultatif (ISO 3166-1 alpha-2, sinon `400 INVALID_INPUT`) : gardé sur le profil local, créé dès l'inscription dans ce cas, puis repris par l'organisation personnelle à sa création (voir [organizations.md](organizations.md)) |
| `POST` | `/login` | Email/mot de passe → session, ou `{mfa_required, challenge}` |
| `POST` | `/login/mfa` | Code du second facteur (application ou code de secours) → session |
| `POST` | `/refresh` | Renouvelle la session via refresh_token |
| `POST` | `/verify-email` | Confirme l'adresse (lien reçu) |
| `POST` | `/forgot-password` | Envoie l'email de réinitialisation |
| `POST` | `/reset-password` | Nouveau mot de passe depuis le lien (toutes les sessions sont fermées) |
| `POST` | `/resend-verification` | Renvoie l'email de vérification |
| `GET` | `/social/providers` | Fournisseurs configurés (Google, GitHub) |
| `GET` | `/social/:provider/authorize` | URL de la page du fournisseur |
| `POST` | `/social/:provider/callback` | Code du fournisseur → session (ou second facteur demandé) |
| `GET` | `/cli/authorize-url` | CLI : page de la console où autoriser la CLI |
| `POST` | `/cli/token` | CLI : code + preuve PKCE → session |
| `POST` | `/device/code` | CLI : démarre une connexion par code d'appareil |
| `POST` | `/device/token` | CLI : session une fois le code validé (`authorization_pending` sinon) |

### Authentifié

| Méthode | Chemin | Description |
|---------|--------|-------------|
| `POST` | `/logout` | Ferme la session (refresh révoqué ; ancien jeton Keycloak : JTI blacklisté) |
| `GET` | `/validate` | Valide le token courant |
| `POST` | `/change-password` | Change le password (nécessite l'ancien) |
| `POST` | `/bootstrap` | Provisionne profil local + projet initial |
| `GET` | `/bootstrap/status` | Vérifie si le bootstrap est fait |
| `POST` | `/session/verify` | Step-up : code du second facteur avant une action sensible |
| `GET` | `/sessions` | Sessions actives (appareils) |
| `DELETE` | `/sessions/:id` | Ferme une session |
| `POST` | `/sessions/revoke-others` | Ferme toutes les autres sessions |
| `GET` | `/mfa` | État du second facteur |
| `POST` | `/mfa/totp` | Démarre l'activation TOTP (secret + QR code) |
| `POST` | `/mfa/totp/confirm` | Confirme (premier code) ; codes de secours remis une fois |
| `DELETE` | `/mfa/totp` | Désactive le second facteur (vérification récente exigée) |
| `POST` | `/mfa/recovery-codes` | Nouveaux codes de secours (vérification récente exigée) |
| `GET` | `/identities` | Comptes Google/GitHub liés |
| `DELETE` | `/identities/:provider` | Délie un compte (il doit rester un moyen de connexion) |
| `POST` | `/cli/authorize` | Autorise la CLI depuis la console (second facteur récent exigé s'il est actif) |
| `GET` | `/device` | Code d'appareil : origine de la demande (IP, appareil) |
| `POST` | `/device/approve` | Valide ou refuse la demande (second facteur récent exigé s'il est actif) |

## Tables DB

`auth_sessions`, `auth_factors`, `auth_recovery_codes`, `auth_email_tokens` (migration 000014) et `user_identities` (000015), gérées par [internal/auth](../../internal/auth/) ; `users` du module `auth`. `user_sessions` / `revoked_tokens` ne servent plus qu'aux sessions Keycloak d'avant la migration.

## Activities Temporal

Aucune. Tous les appels à Keycloak sont synchrones dans le service.

## Invariants critiques

1. **Keycloak reste la source de vérité des mots de passe** (betorun ne les stocke jamais). Si Keycloak dit "invalide", kubemanager refuse.
2. **Rien de Keycloak ne sort du backend** : ni jeton, ni URL, ni message d'erreur. Les clients reçoivent des jetons betorun (`sub` = `user:<users.id>`).
3. **La table `users` miroir Keycloak** via la clé de jointure `keycloak_user_id`. Toujours `upsert` via `FindByKeycloakUserID` avant de créer un user local.
4. **Le grant password n'est jamais offert aux clients** : c'est un appel interne du backend (client confidentiel `auth_client_id` / `auth_client_secret`) vers son annuaire.
5. **Réponses identiques** que le compte existe ou non (inscription, mot de passe oublié, renvoi de vérification).
6. **Transition** : `/refresh` et `/logout` acceptent encore un refresh token Keycloak d'une session ouverte avant les sessions betorun (préfixe différent de `auth.RefreshTokenPrefix`).

## Pitfalls

- **Race condition register** — si deux register concurrents pour le même email arrivent, seule la première réussit côté Keycloak ; `syncLocalUser` refait une lecture sur `unique_violation`.
- **Clock skew** — les jetons ont un `exp` court (10 min) ; si la machine host a une heure décalée, ils sont rejetés. Toujours NTP sync.
- **Session révoquée** — le middleware relit l'état de la session avec un cache de 30 s : une session fermée cesse de fonctionner en 30 s au plus.

## Tests

- [service/bootstrap_status_test.go](../../internal/modules/identity/service/bootstrap_status_test.go) — bootstrap à la première visite.
- [keycloak/client_test.go](../../internal/modules/identity/keycloak/client_test.go) — client Keycloak.
- [internal/auth/totp_test.go](../../internal/auth/totp_test.go), [internal/auth/auth_e2e_test.go](../../internal/auth/auth_e2e_test.go) — TOTP, sessions.
- Tests e2e : [scripts/e2e.sh](../../scripts/e2e.sh) + [scripts/keycloak/get_test_token.sh](../../scripts/keycloak/get_test_token.sh).

## Fichiers clés

- [internal/modules/identity/](../../internal/modules/identity/) — controllers (`hidentity.go`, `hauth.go`, `hdevice.go`) et service (`authflow.go`, `mfa.go`, `emailflow.go`, `socialflow.go`, `cliauth.go`, `service.go`).
- [internal/modules/identity/keycloak/](../../internal/modules/identity/keycloak/) — client Keycloak (admin REST, annuaire `directory.go`).
- [internal/auth/](../../internal/auth/) — sessions, facteurs, jetons d'email, identités liées, fournisseurs sociaux.
- Pour tester localement : [scripts/keycloak/bootstrap_platform.sh](../../scripts/keycloak/bootstrap_platform.sh) crée le realm + client + users.

## Points d'attention

- Les secrets Keycloak admin sont chargés depuis `config.Keycloak.AdminClientSecret` (env `KUBEMANAGER_KEYCLOAK_ADMIN_CLIENT_SECRET`) ; le client de vérification des mots de passe depuis `config.Keycloak.AuthClientID` / `AuthClientSecret`.
- Pour ajouter un IdP social : implémenter le fournisseur dans [internal/auth/social](../../internal/auth/social/) et le déclarer dans la config ; `SocialAuthorizeURL` / `SocialCallback` ([service/socialflow.go](../../internal/modules/identity/service/socialflow.go)) le prennent en charge.
- Plan de retrait de Keycloak de la surface publique : [auth.md §12](../architecture/auth.md) et [interne/keycloak-bootstrap.md](../interne/keycloak-bootstrap.md).
