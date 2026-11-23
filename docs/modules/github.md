# Module `github`

> Connexion GitHub des conteneurs : une GitHub App de betorun est installée sur un compte ou une organisation GitHub, liée à un projet, puis un conteneur suit un dépôt (privé compris) et une branche ; chaque push sur cette branche lance un build qui le redéploie.

## Modèle

Migration 000022 :

- `github_installations (project_id, installation_id)` : installation de l'application liée à un projet (`account_login`, `account_type`, `suspended`, `created_by`). Une même installation peut être liée à plusieurs projets ; le jeton GitHub de la personne qui l'a liée **n'est pas conservé**.
- `workload_git_links` (clé : `workload_id`) : dépôt suivi par un conteneur. Le dépôt est identifié par `repo_id` (un nom change ou peut être repris) ; `repo_full_name` n'est qu'une copie d'affichage. Champs : `branch`, `context_dir`, `dockerfile`, `auto_deploy` (vrai par défaut), `disabled_reason` (vide = lien actif ; sinon `INSTALLATION_DELETED`, `INSTALLATION_SUSPENDED`, `REPOSITORY_REMOVED`).
- `github_webhook_deliveries (delivery_id)` : livraisons déjà traitées (idempotence), purgées après 30 jours.
- Colonnes ajoutées à `builds` : `git_commit`, `git_repo_id`, `git_installation_id`, `deploy_workload_id`, `trigger` (`manual` ou `push`), `deploy_state`.

## Endpoints HTTP

Préfixe `/kmanager/v1`. `:id` = projet (routes `/projects/:id/github`) ou conteneur (routes `/workloads/:id/github`).

`workloads.github.read` (développeur, éditeur, propriétaire, développeur conteneurs) : voir les comptes reliés, leurs dépôts et leurs branches ; c'est une lecture, ouverte au Copilot (`list_github_accounts`, `list_github_repositories`, `list_github_branches`). Relier, délier et déployer restent `workloads.github.manage` (éditeur).

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/integrations/github/status` | authentifié | Fonction disponible sur la plateforme ? |
| POST | `/integrations/github/callback` | authentifié (+ `workloads.github.manage` vérifiée par le service sur le projet de l'état) | Retour de GitHub : lie l'installation au projet |
| POST | `/integrations/github/webhook` | public, signature HMAC | Livraison de GitHub (route interne) |
| GET | `/projects/:id/github/installations` | `workloads.github.read` | Comptes GitHub liés au projet |
| POST | `/projects/:id/github/installations/url` | `workloads.github.manage` | URL d'installation (état valable 10 min, pour l'appelant seul) |
| DELETE | `/projects/:id/github/installations/:installationID` | `workloads.github.manage` | Délier : liens supprimés, conteneurs intacts |
| GET | `/projects/:id/github/installations/:installationID/repositories` | `workloads.github.read` | Dépôts (`q`, `page`, `per_page` ≤ 100) |
| GET | `/projects/:id/github/installations/:installationID/repositories/:repoID/branches` | `workloads.github.read` | Branches (100 au plus) et branche par défaut |
| POST | `/projects/:id/github/workloads` | `workloads.github.manage` (+ `workloads.workloads.create` par le service) | Conteneur + lien + premier build (`202`) |
| GET | `/workloads/:id/github` | `workloads.workloads.get` | Lien, dernier commit déployé, dernier build (outil Copilot `get_workload_github`) |
| PUT | `/workloads/:id/github` | `workloads.github.manage` | Lier ou remplacer le lien (répare un lien désactivé) |
| PATCH | `/workloads/:id/github` | `workloads.github.manage` | Branche, `context_dir`, `dockerfile`, `auto_deploy` |
| DELETE | `/workloads/:id/github` | `workloads.github.manage` | Supprimer le lien (le conteneur tourne toujours) |
| POST | `/workloads/:id/github/deploy` | `workloads.workloads.update` | Construire et déployer le dernier commit de la branche (`202`) |

CLI : `betorun workloads git status <nom>` et `betorun workloads git deploy <nom>` ; la connexion elle-même se fait dans la console (navigateur).

## Déroulé

**Installation.** `POST …/installations/url` génère un état aléatoire (32 octets), stocké dans Redis 10 min sous son empreinte SHA-256, lié à la personne et au projet. Au retour, la console transmet `state`, `installation_id`, `code`, `setup_action` à `/callback`, qui :
1. consomme l'état (usage unique, même personne ; l'état d'un autre n'est pas consommé) ;
2. revérifie `workloads.github.manage` sur le projet ;
3. `setup_action=request` sans installation : renvoie `requested` (demande envoyée à un propriétaire de l'organisation GitHub) ;
4. échange le `code` contre un jeton utilisateur, qui doit lister l'installation (`installation_id` du navigateur n'est pas une preuve) ;
5. exige que la personne voie **tous** les dépôts de l'installation, sinon `ErrInstallationPartial` ;
6. enregistre l'installation (`linked`).

**Jetons d'installation.** Toujours restreints : `metadata: read` pour lister, `contents: read` sur un seul dépôt pour les branches et le clonage. Le refus de GitHub de restreindre au dépôt prouve qu'il n'appartient pas à l'installation. Le jeton de clonage est généré par l'activité de build (`CloneAccess`) au moment de créer le Job ; l'installation doit encore être liée au projet du build, sinon `GIT_ACCESS_LOST`.

**Création depuis un dépôt.** Validation (nom, ref, chemins), résolution de l'installation, du dépôt et du commit de tête, création du conteneur « en attente de build » (quota réservé), lien, puis premier build. Un build refusé (quota) laisse un build `FAILED` visible et le conteneur `FAILED`.

**Webhook.** Corps lu jusqu'à 25 Mo, signature `X-Hub-Signature-256` vérifiée sur le corps brut avant tout décodage, en-têtes `X-GitHub-Delivery` et `X-GitHub-Event` obligatoires, traitement en 8 s au plus. Livraison déjà vue : `duplicate` ; échec du traitement : livraison libérée pour qu'un renvoi de GitHub réussisse. Réponse `{"status": "accepted|ignored|duplicate"}`.
- `push` : branche seulement (pas les tags, ni les suppressions, ni un commit nul) ; liens actifs avec `auto_deploy` dont l'installation n'est pas suspendue, reconnus par `(installation_id, repo_id, branch)`, jamais par le nom. Met à jour le nom du dépôt, puis un build par conteneur (l'échec de l'un ne bloque pas les autres).
- `installation` : `deleted` désactive les liens (`INSTALLATION_DELETED`) et supprime l'installation ; `suspend` / `unsuspend` basculent `INSTALLATION_SUSPENDED`.
- `installation_repositories` : `removed` / `added` basculent `REPOSITORY_REMOVED`.

## Configuration

Section `github_app` ([config.go](../../internal/core/configs/config.go)), secrets par l'environnement (`KUBEMANAGER_GITHUB_APP_*`), clé privée dans un fichier hors du code et de la base : `app_id`, `slug`, `private_key_file`, `webhook_secret`, `client_id`, `client_secret` (tous obligatoires), `api_url` et `web_url` (défaut github.com ; `web_url` sert aussi à construire l'URL de clonage). Configuration incomplète, clé illisible ou builds désactivés : la fonction est désactivée sans empêcher l'API de démarrer (`status.enabled = false`, webhook en `404 GITHUB_DISABLED`, `GET /workloads/:id/github` répond vide).

Côté GitHub : URL du webhook `<API>/kmanager/v1/integrations/github/webhook`, événements `push`, `installation`, `installation_repositories`.

## Pièges

- **`betorun.yaml` n'est pas lu ici** : c'est le fichier de projet de la CLI ([projectfile.go](../../internal/cli/projectfile.go)). Un build déclenché par push utilise uniquement les réglages du lien (`context_dir`, `dockerfile`) et la détection automatique des builds.
- Les routes de connexion exigent une **personne** : un compte de service reçoit `ErrInstallationDenied`.
- Supprimer l'installation côté GitHub désactive les liens sans les effacer ; il faut un `PUT /workloads/:id/github` après reconnexion. `DELETE …/installations/:id` côté betorun, lui, supprime les liens.
- Changer de branche est refusé sur un lien désactivé (`ErrLinkDisabled`).
- La recherche de dépôts se fait côté API, sur 1 000 dépôts au plus (10 pages de 100).
- Le projet d'un lien vient toujours du conteneur, jamais de la requête.

## Fichiers clés

- [github.go](../../internal/modules/github/github.go) — contrôleur, webhook (taille, signature, en-têtes).
- [service/service.go](../../internal/modules/github/service/service.go) — installation, liens, builds, traitement des événements.
- [client/client.go](../../internal/modules/github/client/client.go) — JWT de l'application, jetons restreints, API GitHub, `SignatureValid`.
- [repository/repository.go](../../internal/modules/github/repository/repository.go) — tables, désactivation des liens, idempotence.
- [domain/github.go](../../internal/modules/github/domain/github.go) — modèles, erreurs, requêtes.
- [github_wiring.go](../../internal/infrastructure/router/github_wiring.go), [route.go](../../internal/infrastructure/router/route.go) (`addGitHubRoutes`) — câblage.
- [000022_github_connection.up.sql](../../migrations/000022_github_connection.up.sql) — schéma.
