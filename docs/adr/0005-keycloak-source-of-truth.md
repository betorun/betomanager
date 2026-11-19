# ADR 0005 — Keycloak comme source unique d'authentification

- **Status** : accepté
- **Date** : 2025-11-30

## Contexte

KubeManager a besoin d'authentifier des utilisateurs via plusieurs flows :

- Register / login email+password.
- Social OAuth (Google, GitHub).
- Refresh token rotation.
- Reset password.
- API admin pour gérer les users (reset, disable, groupes).

On pouvait :

- Implémenter l'auth maison (stockage password BCrypt, endpoints REST, OAuth flows, ...).
- Utiliser **Keycloak** (ou Auth0/Ory/...) comme fournisseur d'identité externe.

## Décision

**Keycloak** comme source unique. Kubemanager :

- **Ne stocke jamais de password**.
- **Délègue tous les flows auth** (register, login ROPC, refresh, OAuth social, reset) à Keycloak.
- Valide les JWT localement via JWKS.
- Maintient une **table locale `users`** miroir de Keycloak via `keycloak_user_id`.

## Conséquences

### Positives

- **Zéro gestion de password** côté kubemanager. Les best practices (BCrypt, salt, OAuth flows RFC-compliant, password policy) sont gérées par Keycloak qui sait le faire depuis 15 ans.
- **OAuth social facile** : configurer un IdP dans Keycloak admin console, pas de code à écrire.
- **SSO éventuel** : si demain on a d'autres apps internes, elles partagent Keycloak → SSO natif.
- **Admin REST API riche** : reset password, disable user, manage groups, roles — tout est exposé par Keycloak.
- **Audit trail** : Keycloak log tous les logins, reset, etc. — satisfait des exigences compliance.
- **JWT stateless** : kubemanager valide les tokens via JWKS, pas de session DB → latence d'auth minimale.

### Négatives

- **Keycloak = SPOF critique**. Keycloak down = personne ne peut login (mais les tokens actifs continuent à fonctionner jusqu'à expiration).
- **Complexité opérationnelle** : Keycloak est une JVM avec sa propre Postgres, ses upgrades, son HA — un service de plus à opérer.
- **Customisation limitée** : l'UI Keycloak de register/login est peu extensible sans théme. Alternative : écrire notre propre UI et utiliser Keycloak en pur backend.
- **ROPC en dev uniquement** : on a un client dédié `kubemanager-dev-cli` avec `directAccessGrants: true` pour les scripts de test. **Ce client ne doit PAS exister en prod**.
- **Latence d'un appel admin** : chaque `resetPassword` appelle Keycloak REST API → ~100-300 ms par appel. Négligeable mais pas gratuit.

## Alternatives considérées

### Auth maison

Rejeté parce qu'on ferait moins bien que Keycloak en ayant plus de dette. Les bugs d'auth (timing attacks, replay, CSRF OAuth…) sont très dangereux.

### Auth0 (SaaS)

Excellent produit, mais :

- **Coût** variable en fonction du nombre d'utilisateurs — peut exploser pour un produit B2G avec des millions d'users.
- Dépendance externe sur un SaaS → problématique pour certains environnements réglementés.

Peut être reconsidéré si on pivote B2C grand public.

### Ory (Kratos + Hydra)

Aussi excellent. Plus léger que Keycloak (pas de JVM), plus "cloud native". Rejeté pour l'instant parce que :

- L'équipe a déjà une expertise Keycloak (gophercart l'utilise).
- La stack est moins mature (moins de StackOverflow, moins de plugins).

À reconsidérer dans 1-2 ans si Ory devient plus populaire / Keycloak moins maintenu.

### Supabase Auth

Rejeté — Supabase est couplé à Postgres, et nous on veut garder Postgres kubemanager pur métier (pas d'auth mixée dedans).

## Flux

### Register
```
Client → POST /identity/register
→ Keycloak admin REST (crée user)
→ kubemanager UPSERT users (miroir local)
→ email verification envoyé par Keycloak
```

### Login (ROPC dev)
```
Client → POST /identity/login {email, password}
→ Keycloak token endpoint /protocol/openid-connect/token (grant=password)
→ renvoie {access_token JWT, refresh_token}
→ kubemanager stocke refresh_token hashé dans user_sessions
```

### Login (OAuth social)
```
Client → GET /identity/social/google
→ redirect vers Keycloak /auth (provider=google)
→ Google OAuth flow
→ Keycloak redirect vers /identity/social/callback
→ kubemanager récupère le code, exchange, upsert user local
```

### Validation d'un token (chaque requête)
```
Client → (toute route protégée) avec Authorization: Bearer <jwt>
→ middleware récupère JWKS Keycloak (cache 5 min)
→ vérifie signature RS256, iss, aud, exp
→ pose userID = claim sub dans ctx
```

## Contraintes opérationnelles

- **JWKS cache** : récupérer la clé publique Keycloak à chaque requête serait trop lent. Cache avec rotation automatique côté JWT lib.
- **Clock skew** : tolérance ~30 s sur `exp` — au-delà, les JWT sont rejetés. Toutes les machines doivent être NTP sync.
- **Keycloak upgrade** : tester en staging, le schéma DB change parfois entre versions majeures.
- **Backup** : la DB Keycloak doit être backup-ée indépendamment. Perdre Keycloak = perdre tous les comptes utilisateurs.

## Références

- Module : [identity](../modules/identity.md)
- Guide bootstrap : [interne/keycloak-bootstrap.md](../interne/keycloak-bootstrap.md)
- Keycloak docs : <https://www.keycloak.org/documentation>
