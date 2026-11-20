# Authentification des personnes : Keycloak invisible

> Statut : **phases 1, 2 et 3 réalisées** (phase 3 le 2026-11-20 : passkeys, retrait des flux Keycloak, Keycloak en réseau privé). Reste : essai des passkeys dans un vrai navigateur et sur téléphone.
> Décision associée : [ADR 0007](../adr/0007-keycloak-behind-backend.md) (amende l'ADR 0005).

## 1. Exigence

La console web ne montre **jamais** rien de Keycloak : ni page, ni URL, ni email, ni jeton, ni message d'erreur. Personne ne doit pouvoir deviner que Keycloak gère les comptes. **Next.js est seul à afficher des pages** ; **le backend est seul à parler à Keycloak**.

Conséquences directes :

- Keycloak devient un **annuaire privé** (comptes, mots de passe), joignable uniquement par le backend. Il n'est plus exposé sur Internet.
- Le backend émet **ses propres jetons** (émetteur `https://api.betorun.app/…`, signés par les clés du STS). Aucun jeton Keycloak ne sort du backend.
- La double authentification, les sessions, les emails et la connexion Google/GitHub sont gérés **par le backend**, avec des écrans Next.js.

Les principes de sécurité restent ceux d'un fournisseur cloud : mots de passe jamais stockés par betorun (ils restent dans Keycloak), jetons courts, sessions révocables, MFA, protection contre le bourrage d'identifiants, audit.

## 2. Ce qui expose Keycloak aujourd'hui

| # | Surface | Où | Ce qu'on voit |
|---|---|---|---|
| 1 | Connexion web | `GET /identity/oidc/authorize` → redirection | Page de connexion Keycloak |
| 2 | Google / GitHub | `GET /identity/social/:provider` (kc_idp_hint) | Passage par l'URL Keycloak, pages « premier login » possibles |
| 3 | Double authentification, step-up | Flux `betorun-browser` (ajouté aujourd'hui) | Pages OTP Keycloak |
| 4 | Gestion des facteurs | `account_security_url` (page Sécurité) | Console de compte Keycloak |
| 5 | Vérification d'email, mot de passe oublié | `forgot-password`, `resend-verification` | Emails Keycloak, liens vers ses pages |
| 6 | CLI : `betorun login` | Flux device, page de vérification | Page `/device` Keycloak |
| 7 | CLI : step-up | Retour local 127.0.0.1 | Pages de connexion et OTP Keycloak |
| 8 | Jetons | Console et CLI reçoivent les jetons Keycloak | `iss` = URL Keycloak, `realm_access`, `azp`, `sid`… |
| 9 | Erreurs | Messages relayés (« Invalid user credentials »…) | Vocabulaire Keycloak |
| 10 | Réseau | Keycloak publié (client public `kubemanager-web`, `betorun-cli`) | Domaine Keycloak joignable |

Tout est à remplacer.

## 3. Architecture cible

```
Navigateur ──► Next.js (pages + route handlers /api/auth/*) ──► API betorun ──► Keycloak (réseau privé)
CLI ─────────────────────────────────────────────────────────► API betorun ──► Google / GitHub (OAuth)
                                                                    │
                                                                    └─► SMTP (emails betorun)
```

- **Next.js** affiche toutes les pages et garde la session web dans un cookie `HttpOnly` sur son propre domaine (patron *backend for frontend*). Le jeton d'accès vit en mémoire, jamais dans `localStorage`.
- **API betorun** (module `identity`) : connexion, MFA, sessions, emails, OAuth Google/GitHub, autorisation de la CLI. Elle appelle Keycloak en serveur à serveur.
- **Keycloak** : comptes et mots de passe (vérification, changement), désactivation. Plus aucun client public, plus de thème, plus d'emails.

## 4. Sessions et jetons

### 4.1 Session

Table `auth_sessions` : `id`, `user_id`, `kind` (web | cli), `created_at`, `last_used_at`, `expires_at` (absolue, 30 j web, 90 j CLI), `idle_expires_at` (7 j web), `ip`, `user_agent`, `auth_time`, `mfa_at`, `amr` (pwd, otp, google…), `revoked_at`, `revoked_reason`.

- **Jeton de rafraîchissement** opaque (256 bits), stocké **haché**, **tournant** à chaque usage. Un jeton déjà utilisé qui revient = vol présumé : toute la session est révoquée (détection de réutilisation).
- Révocation : par l'utilisateur (page Sécurité), « se déconnecter partout », changement ou réinitialisation de mot de passe, désactivation du compte, admin de l'organisation.

### 4.2 Jeton d'accès

JWT ES256 de **10 minutes**, signé par les clés du STS (mêmes JWKS) :

| Claim | Valeur |
|---|---|
| `iss` | émetteur public de l'API |
| `aud` | `betorun-api` |
| `sub` | `user:<users.id>` (identifiant interne, jamais l'identifiant Keycloak) |
| `sid` | session |
| `auth_time` | dernière authentification |
| `amr` | méthodes (`pwd`, `otp`, `google`…) |
| `acr` | `silver` (un facteur) ou `gold` (MFA) |

Le middleware vérifie la signature puis l'état de la session (cache 30 s, comme les comptes de service) : une session révoquée cesse de fonctionner en 30 s au plus. Le step-up et `iam.requireMfa` restent tels quels : ils lisent `acr` et `auth_time`, désormais posés par betorun.

## 5. Connexion par mot de passe

1. `POST /identity/login {email, password}` : le backend vérifie le mot de passe auprès de Keycloak (client confidentiel privé, réseau interne ; les jetons Keycloak sont jetés aussitôt).
2. Sans MFA : session ouverte (`acr=silver`). Avec MFA : réponse `{mfa_required, challenge}` (défi de 5 min, usage unique).
3. `POST /identity/login/mfa {challenge, code}` : session ouverte avec `acr=gold`.

Protections :

- **Limites** par IP et par compte (Redis, ADR 0006), plus la détection de force brute de Keycloak ;
- **réponse identique** que l'email existe ou non (anti-énumération), délai constant ;
- **captcha** (Turnstile) après plusieurs échecs ;
- journal d'audit : succès, échecs, verrouillages.

Le grant « password » n'est **jamais** offert aux clients : c'est un appel interne du backend vers son annuaire. Les clients utilisent l'API betorun, pas OAuth Keycloak.

## 6. Double authentification gérée par betorun

- **TOTP** (RFC 6238) : le backend génère le secret, chiffré en base (AES-256-GCM, même clé de chiffrement que le STS, remplaçable par un KMS). Next.js affiche le QR code ; activation après un premier code correct.
- **Codes de secours** : 10 codes à usage unique, hachés, affichés une seule fois.
- **Passkeys** (WebAuthn, `internal/auth/passkeys.go`, table `auth_passkeys`, migration 000053). Jusqu'à 10 par compte.
  - *Rôle* : second facteur à part entière, et connexion sans mot de passe (bouton « Se connecter avec une passkey », passkey choisie par le navigateur).
  - *Règles* : vérification de l'utilisateur exigée (empreinte, visage, code de l'appareil), sans attestation ; RP ID = domaine de la console, origine = adresse de la console (`frontend_base_url`). Compteur de signature contrôlé (clone refusé).
  - *Cérémonies* : défi gardé 5 min dans Redis, à usage unique, lié à son usage (enregistrement, connexion, second facteur, step-up).
  - *Ajout* : exige une vérification récente si un facteur existe déjà. Première passkey sans TOTP : codes de secours remis à ce moment. Retrait du dernier facteur : codes de secours supprimés. Emails de sécurité à l'ajout et au retrait.
  - *Routes* : `POST /identity/login/passkey{,/options}`, `/identity/login/mfa/passkey{,/options}`, `/identity/session/verify/passkey{,/options}`, `GET|POST /identity/passkeys`, `POST /identity/passkeys/options`, `PATCH|DELETE /identity/passkeys/:id`. La réponse `mfa_required` indique les méthodes possibles (`totp`, `passkey`, `recovery`).
- **Step-up** : `POST /identity/session/verify {code}` met à jour `mfa_at` et renvoie un jeton `gold`. Dans la console, une fenêtre Next.js demande le code puis rejoue l'action. Dans la CLI, invite dans le terminal ou page Next.js.
- **Politique d'organisation** `requireMfa` : inchangée. Un membre sans facteur est conduit à l'écran d'enrôlement Next.js.
- Les OTP enregistrés dans Keycloak aujourd'hui (dev uniquement) ne sont pas repris : réenrôlement.

## 7. Inscription, email, mot de passe

- **Module d'envoi d'emails** du backend : SMTP (MailHog en dev), modèles FR/EN aux couleurs de betorun, expéditeur `no-reply@betorun…`.
- **Vérification d'email** et **mot de passe oublié** : jetons à usage unique, hachés, 24 h et 1 h. Liens vers des pages Next.js (`/verify-email`, `/reset-password`). Le backend applique le résultat via l'API admin Keycloak (`emailVerified`, `reset-password`) et révoque les sessions après une réinitialisation.
- **Changement de mot de passe** : ancien mot de passe vérifié, sessions des autres appareils révoquées.
- Réponses identiques que le compte existe ou non.
- **E-mails de sécurité** (au titulaire, hors du chemin de la requête, au mieux ; lien `/settings#sessions`) :
  - **nouvelle connexion** depuis un appareil jamais vu (table `user_devices`, migration 000048 : famille client · navigateur · système tirée du User-Agent, pas l'adresse IP ; la toute première connexion d'un compte n'envoie rien). L'e-mail donne l'appareil, l'adresse IP et l'heure (UTC). Limite : deux machines de même navigateur et système se confondent (pas de cookie d'appareil) ;
  - **code de secours utilisé** pour se connecter (remplace l'e-mail d'appareil pour cette connexion) ;
  - double authentification **activée** ou **désactivée** ; **nouveaux codes de secours** ;
  - **compte Google ou GitHub lié** à un compte existant (pas à la création d'un compte par Google/GitHub).
  La CLI envoie `betorun-cli (<système>)` pour que l'appareil soit nommé.

## 8. Google et GitHub

Le backend parle **directement** à Google et GitHub (OAuth 2.0 / OIDC, `state` + PKCE). Keycloak n'est plus dans le chemin du navigateur.

- Page de retour Next.js (`/auth/callback/google`) qui transmet le code au backend.
- Table `user_identities (provider, subject, user_id)`.
- Rattachement automatique à un compte existant **uniquement** si le fournisseur atteste l'email (vérifié) ; sinon, confirmation par mot de passe.
- Nouveau compte : créé dans Keycloak (sans mot de passe) puis profil local, comme l'inscription.

## 9. CLI

- `betorun login` (navigateur) : la CLI ouvre une page Next.js `/cli/authorize` (PKCE, retour `127.0.0.1:<port>`). L'utilisateur, connecté à la console, confirme ; le backend remet un code à usage unique lié au PKCE ; la CLI l'échange contre une session `kind=cli`.
- `betorun login --device` (machine sans navigateur) : codes device gérés par le backend (RFC 8628), page Next.js `/device`.
- Step-up dans la CLI : code TOTP demandé dans le terminal (plus simple que le navigateur).
- Les identifiants machine (fichier de clé, OIDC de CI, conteneur) ne changent pas : ils passent déjà par le STS.

## 10. Page Sécurité (Next.js)

Sessions actives (appareil, IP, dernière activité) et révocation ; « se déconnecter partout » ; mot de passe ; facteurs (TOTP, codes de secours, passkeys) ; comptes Google/GitHub liés ; historique des connexions (journal d'audit).

## 11. Keycloak après la migration

- Réseau privé uniquement : en prod, réseau Docker `backend` interne, aucun port publié, `KC_HOSTNAME` interne. Seule l'API le joint.
- Un seul client utile : confidentiel, `betorun-auth`, réservé au backend (vérification des mots de passe) ; le client admin existant pour l'API admin.
- Clients publics `kubemanager-web` et `betorun-cli` supprimés par `scripts/keycloak/bootstrap_platform.sh` s'ils existent ; flux `betorun-browser` plus créé en prod, SMTP Keycloak désactivé.
- Côté API, plus de relais Keycloak : déconnexion, rafraîchissement, emails d'action et réglages `web_client_id` / `cli_client_id` retirés. Un ancien jeton de rafraîchissement renvoie « session expirée ».
- Un jeton Keycloak présenté directement à l'API est refusé (`keycloak.accept_user_tokens`, faux par défaut). Seuls les scripts de test locaux l'activent, avec le client de test `kubemanager-dev-cli`.
- Remplaçable : tout passe par une interface `Directory` (vérifier un mot de passe, créer, désactiver, changer le mot de passe). Une autre implémentation (mots de passe argon2id gérés par betorun, autre fournisseur) pourra la remplacer sans toucher au reste.

## 12. Plan

| Phase | Contenu | Visible |
|---|---|---|
| **1** | Sessions et jetons betorun ; connexion par mot de passe ; MFA TOTP et codes de secours ; step-up ; emails (vérification, mot de passe oublié) ; pages Next.js ; middleware acceptant les deux types de jetons pendant la transition | Plus aucune page Keycloak dans la console |
| **2** | Google/GitHub en direct ; CLI via pages Next.js et device maison ; page Sécurité complète (sessions) | Plus rien de Keycloak nulle part |
| **3** ✅ | Retrait des clients publics et des flux Keycloak, Keycloak en réseau privé ; passkeys | Surface réduite |

Chaque phase est livrée testée de bout en bout sur la stack de dev, comme l'étape 1.

## 13. Impact sur le travail récent

- **Flux MFA Keycloak** (`ensure_mfa_loa`, commit `db163c1`) : rendu inutile par la section 6 ; plus activé en prod depuis la phase 3 (fonction gardée dans le script, désactivée par défaut).
- **`POST /identity/login`** : l'ancienne route (retirée aujourd'hui, non commité) relayait les jetons Keycloak tels quels. Elle est remplacée par la route de la section 5, qui émet des jetons betorun et gère le MFA.
- **Step-up CLI par navigateur** (commit `9ddf13d`) : remplacé par la saisie du code dans le terminal (section 9).
- `users.keycloak_user_id` reste en base, à usage interne uniquement.

## 14. Points ouverts

- **Captcha** : Cloudflare Turnstile (gratuit, sans traque publicitaire) recommandé.
- **Délivrabilité des emails** : domaine d'envoi, SPF, DKIM, DMARC à configurer en prod (ou service transactionnel).
- **Applications Google et GitHub** : à créer au nom de betorun (identifiants OAuth).
- **SMS** comme second facteur : déconseillé (SIM swapping), mais très répandu en Afrique ; à reconsidérer plus tard avec un fournisseur local, en plus du TOTP, pas à sa place.
