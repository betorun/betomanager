# Module `copilot`

> Agent conversationnel de la console : il lit l'état réel des ressources de la personne avec les routes mêmes de l'API, répond en flux (SSE) et ne fait une écriture qu'après validation d'une carte. Désactivé par défaut (`llm.enabled=false`).

## Architecture

- **Couche modèle** : `internal/infrastructure/llm` ([ADR 0008](../adr/0008-llm-provider-layer.md)). Contrat `llm.Provider` (`Generate`, `Stream`), appel d'outils natif uniquement (aucun appel d'outil n'est cherché dans le texte du modèle). Tout appel rendu est vérifié par `CheckToolCalls` (nom déclaré, arguments en objet JSON), sinon `KindMalformedOutput`. Pile assemblée par `llmsetup` : `Instrument(Chain(Breaker(Budget(primary)), Breaker(Budget(fallback))))`. Adaptateur unique : `openai` (toute API « Chat Completions » : vLLM, Groq, Gemini compatible, OpenAI, Mistral…).
- **Agent** (`agent/`) : boucle modèle → outils → modèle, avec des freins par tour (`agent.Limits`) : 6 appels au modèle, 12 appels d'outils, 120 s, 1500 jetons de sortie par appel (défauts).
- **Outils** (`tools/`) : un outil est une route de l'API, pas du code à part.
- **Mémoire** (`store/`) : conversations courtes dans Redis.
- **Propositions** (`proposals.go`, `actions.go`) : cartes d'action dans Redis.

## Des routes aux outils

Une route devient un outil en étant marquée dans la table de routes (`internal/infrastructure/router/`) :

```go
WorkloadGroup.AddRoute(...).
    ForCopilot(CopilotTool{Name: "stop_workload", Write: true, Risk: tools.RiskMedium, Undo: "start_workload", ...})
```

Champs de `CopilotTool` ([copilot_wiring.go](../../internal/infrastructure/router/copilot_wiring.go)) : `Name` (snake_case), `Fixed` (paramètres de requête imposés, invisibles du modèle, ex. `follow=false` sur les journaux), `Max` (plafond d'un entier, ex. `tail ≤ 300`), `Hint` (précision pour le modèle), `Write`, `Risk`, `Omit` (champs du corps cachés, ex. `secrets`), `Only` (liste blanche du corps, prioritaire), `Undo` (outil inverse, mêmes paramètres de chemin).

Une fois toutes les routes enregistrées, `bindCopilot` parcourt les groupes (`CopilotRoutes`), génère la spécification OpenAPI **publique en anglais** et appelle `tools.Build` : le schéma de chaque outil (paramètres de chemin et de requête, corps, descriptions) vient de l'opération correspondante. Une route marquée mais absente de la spécification publique fait échouer la construction. Règles vérifiées (`checkRoute`) :

- une lecture est un `GET` ;
- une écriture n'est jamais un `GET` ;
- une suppression ou une écriture dont la permission est sensible (`Sensitive` dans `internal/iam/permissions.go`) n'est qu'une carte sensible (`Strong`, risque `high`, voir lot A4) ; `ConfirmName` exige un `TargetTool` de lecture dont les paramètres de chemin sont fournis par la carte ;
- une autre écriture déclare un risque `low` ou `medium` ; un `Undo` doit viser une écriture aux mêmes paramètres de chemin.

Un échec de construction désactive le Copilot (erreur journalisée, l'API démarre quand même). S'y ajoute un outil composite, `diagnose_workload` ([diagnose.go](../../internal/modules/copilot/tools/diagnose.go)) : fiche, état en direct, événements et fin des journaux d'un conteneur, chaque morceau passant par sa route.

**Couverture (lot A1)** : chaque route publique a une décision, outil, guide ou hors Copilot ([copilot_decisions.go](../../internal/infrastructure/router/copilot_decisions.go)). Une route déjà marquée `ForCopilot` dans son câblage garde son outil ; les autres sont décidées dans la table `copilotDecisions` (« MÉTHODE chemin » sans `/kmanager/v1`), appliquée à la fin de `AddAllRoutes`. 169 outils (dont 41 cartes sensibles), 62 guides, 44 hors Copilot (275 routes publiques, novembre 2026), plus les outils composites `diagnose_workload`, `find_features` et `propose_launch_plan`.
- **Guide** (`CopilotGuide{Screen, Note}`) : le Copilot connaît la route et envoie vers l'écran de la console (`consoleScreens`) sans l'exécuter : action sensible, suppression, envoi de fichier, secret montré une fois, compte de la personne (routes `Authenticated()`). Les dépôts GitHub ne sont plus des guides : leur lecture a sa permission, `workloads.github.read` (lot B). `CopilotGuideRoutes` les liste (carte des fonctionnalités, lot A2).
- **Hors Copilot** : parcours de connexion et mécanique technique (jetons, découverte, temps réel).
- `TestCopilotCoverage` refuse : une route publique sans décision ; une décision sans route ; un écran inconnu ; un outil sur une route `Authenticated()` (jeton délégué refusé) ; un outil de lecture dont la permission n'est pas une lecture (refusé en portée `read`). Une fonctionnalité nouvelle ne peut donc ni échapper au Copilot ni lui rester inconnue.

## Cartes sensibles : double authentification de la personne (lot A4)

Une suppression ou une action sensible (permission `Sensitive` : données remplacées, argent, accès) n'est jamais jouée avec un jeton délégué : c'est une **carte sensible** (`CopilotTool{Strong: true}`, risque `high`, déclarée par `asStrong` dans la table de couverture).

- **Proposition** : comme toute carte ; la cible est relue avec les droits de la personne par l'outil déclaré (`TargetTool`, ex. `get_database`). `ConfirmName` : la personne retape ce nom (suppressions de ressources entières, données remplacées) ; nom illisible : pas de carte.
- **Validation** (`POST /copilot/actions/:token/approve`, corps facultatif `{confirm}`) :
  1. nom retapé faux → `400 CONFIRMATION_MISMATCH`, la carte reste validable (rien n'est réservé) ;
  2. l'action est jouée avec **la session de la personne** (son jeton Bearer), jamais un jeton délégué : le garde IAM de la route exige alors sa confirmation récente, comme dans la console ;
  3. confirmation manquante ou trop ancienne → la réponse `401 STEP_UP_REQUIRED` de la route est rendue telle quelle (`max_age`…), la carte est libérée (`ProposalStore.Release`) ; l'intercepteur de la console ouvre sa fenêtre de double authentification puis **rejoue la validation** ; annulée : la carte reste validable (« Confirmation annulée : rien n'a été fait ») ;
  4. réussie : jouée une fois (clé d'idempotence = jeton de la carte ; un refus ne la consomme pas), note `validated with their two-factor confirmation`.
- Le Copilot ne demande **jamais** un code, un mot de passe ou un secret dans la conversation (prompt) : la console le fait elle-même.
- Restent des guides : recharge par mobile money, commande et réinstallation de VPS, envois de fichiers, création de clés ou secrets montrés une fois, compte de la personne, politiques IAM brutes, rôles personnalisés, politiques de refus, connexion d'entreprise.
- Console : badge « Double authentification », bordure rouge, champ « Pour confirmer, tapez « nom » » ; bouton de validation actif seulement si le nom correspond.

## Carte des fonctionnalités et noyau d'outils (lot A2)

- **`find_features`** ([tools/features.go](../../internal/modules/copilot/tools/features.go)) : la carte de tout ce que betorun sait faire, générée au démarrage (aucun texte écrit à la main) : chaque outil (lecture ou carte d'action) et chaque route « guide » (écran de la console, double authentification, note), avec son résumé anglais (spécification publique), sa description française (la route) et sa section ; plus les écrans de la console (`consoleScreens`, titres anglais et français). Recherche par mots-clés sans accents, au singulier, en français ou en anglais ; 15 résultats au plus. Sans correspondance : `NO MATCH`, avec la consigne de dire que betorun ne le propose pas et de proposer un ticket. Requête vide : les sections et tous les écrans.
- **Noyau** (`tools.CoreTools`, 26 outils dont les 3 lectures GitHub et `propose_launch_plan`, ~3 500 jetons) : seuls ces outils sont présentés au modèle en début de tour. Les outils que `find_features` nomme (`read tool: x`, `action card: x`) lui sont ajoutés pour la suite du tour (`agent.ScopedRunner`) : jamais le catalogue entier (~127 outils, ~15 000 jetons à chaque appel, au-delà de la limite de 128 outils de certains fournisseurs). Un outil du noyau absent du catalogue fait échouer `TestCopilotFeatureMap`.
- **Prompt** : règle « zéro invention » en tête (tout fait vient d'un outil ; `find_features` avant d'affirmer qu'une fonctionnalité existe ou de citer un écran ; prix seulement par `get_price_catalog` / `get_billing_estimate` ; ticket de support quand la personne est bloquée).

## Vérification des réponses (lot A3)

[grounding](../../internal/modules/copilot/grounding/grounding.go) : chaque élément vérifiable d'une réponse doit avoir une **preuve**.

- **Preuves** (`grounding.Evidence`) : jetons extraits (identifiants, adresses IP, nombres normalisés, hôtes) des résultats d'outils du tour, des cartes proposées, du message et des pièces jointes, de la page, des échanges gardés, et des tours précédents (`betorun:copilot:evidence:<personne>:<conversation>`, mêmes 24 h que la conversation, effacés avec elle). Jamais le texte des résultats : la mémoire ne devient pas un second stockage des journaux ou des configurations.
- **Contrôles** (`grounding.Check`) : identifiant (UUID) ; adresse IPv4 (sauf `0.0.0.0` et `127.0.0.1`, conseils généraux) ; montant accolé à une monnaie (XAF, FCFA, EUR, €, USD, $ ; « 7 300 », « 7300 », « 2 500,5 » et « 2500.5 » se valent) ; lien relatif vers la console (écran de `consoleScreens`, ou page d'une ressource `/workloads/`, `/builds/`, `/sites/`, `/vps/`, `/support/` dont l'identifiant est prouvé) ; adresse web (hôte prouvé) ; nom d'outil interne (jamais montré).
- **Correction** : un élément sans preuve → événement SSE `revising`, le modèle réécrit une fois sa réponse avec la liste (`grounding.Feedback` ; il peut relire un outil) ; ce qui reste sans preuve est retiré (`grounding.Sanitize` : lien → son texte, valeur → « (non vérifié) ») ; la console remplace le brouillon par la réponse vérifiée (événement `replace {answer}`). Métriques `kubemanager_copilot_grounding_total{outcome=clean|revised|sanitized}` et `grounding_violations_total{kind}`.
- Limite : le brouillon est diffusé en direct avant la vérification ; la console le masque dès `revising` (« Je vérifie ma réponse… ») et affiche la version vérifiée.

## Contrôle des droits

Aucun contrôle n'est dupliqué dans le Copilot : chaque outil est rejoué en interne sur le moteur HTTP de l'API (`tools.Invoker`) avec un **jeton délégué** de la personne (`sts.MintDelegated`, méthode `delegated`, acteur `betorun:copilot`, 15 min au plus, révoqué avec la session). Garde IAM, quotas et audit s'appliquent donc comme pour la console.

- **Portée `read`** pour la conversation : le garde (`checkDelegated`) refuse toute permission qui n'est pas en lecture (`DELEGATION_SCOPE`).
- **Portée `write`** (2 min) seulement à la validation d'une carte.
- Une permission sensible est toujours refusée par délégation (`ACTION_REQUIRES_PERSON`) : le jeton délégué ne porte ni `auth_time` ni `amr`.
- Les routes `Authenticated()` refusent les jetons délégués (`RefuseDelegated`) ; seules les routes `AuthenticatedDelegable` les acceptent.
- Audit : le principal reste la personne ; l'entrée porte `actor` et `delegation_scope` dans ses métadonnées.

## Écritures : cartes de validation

Le modèle ne fait jamais d'écriture : il la **propose**. Déroulé :

1. `Catalog.Prepare` fige la requête (méthode, chemin échappé, corps filtré).
2. La ressource visée est relue avec les droits de la personne (`get_workload`, `get_site`, `get_database`, `get_build`) ; introuvable ou interdite : pas de carte. Pour une création, le nom suffit.
3. La proposition (jeton aléatoire de 32 octets) est gardée 10 min dans Redis (`betorun:copilot:action:<jeton>`) et envoyée à la console (événement `action` : outil, risque, cible, détails hors identifiants de chemin). Une seule carte par message.
4. Le modèle reçoit `awaiting_validation` : rien n'est fait.
5. `POST /copilot/actions/:token/approve` : la carte est réservée (`SETNX`, une seule issue même avec deux onglets ou deux instances), jouée une fois avec un jeton délégué `write`, jamais rejouée en cas d'échec. Réponse : `done` ou `failed`, le résultat JSON de la route (8 Kio au plus) et, si l'outil a un inverse, une carte d'annulation valable 30 min.
6. `POST /copilot/actions/:token/reject` : rien n'est fait.

Dans les deux cas, une note est ajoutée à la conversation pour le tour suivant. Une carte d'autrui est vue comme « introuvable » (`410 ACTION_EXPIRED`) ; déjà traitée : `409 ACTION_ALREADY_HANDLED`.

## Plan « mettre en ligne » (lot B)

« Mets mon appli Next.js en ligne avec une base » : le Copilot lit les dépôts GitHub de la personne (`list_github_accounts`, `list_github_repositories`, `list_github_branches`, permission `workloads.github.read`), puis propose **une seule carte** ; une validation joue toutes les étapes à la suite, côté serveur, avec suivi en direct.

- **Outil** `propose_launch_plan` ([tools/launch.go](../../internal/modules/copilot/tools/launch.go)) : écriture composite (pas une route), ajoutée au catalogue seulement si tous les outils-routes dont elle dépend y sont (`tools.LaunchPlanDeps` ; `TestCopilotFeatureMap` vérifie qu'elle l'est en vrai). Arguments stricts (champs inconnus refusés à tous les niveaux) : `project_id`, `name` (3-30), `github {installation_id, repository (full_name ou identifiant), branch, context_dir?, dockerfile?}`, `target_port?` (seulement s'il est lu dans le dépôt), `kind?` (`web` par défaut, ou `worker`), `database? {name}`, `addon? {template: redis|mariadb|meilisearch, name}`, `domain? {hostname}` (conteneur web seulement). Le modèle ne donne que des paramètres : **le serveur construit les étapes** ([service/launchplan.go](../../internal/modules/copilot/service/launchplan.go)).
- **Proposition** (`BoundRunner.proposePlan`) : compte GitHub relié au projet et non suspendu, dépôt trouvé dans ce compte (recherche par nom ou pages de 30, 10 au plus), branche existante — relus avec le jeton `read` de la personne ; introuvable : pas de carte (zéro invention). Chaque requête préparable passe `Catalog.Prepare` (identifiants à venir remplacés par un UUID nul) : des arguments faux sont refusés avant la carte. Une seule proposition par message (même compteur que les cartes). La carte est une preuve (grounding), comme une carte d'action.
- **Étapes**, dans cet ordre et seulement celles demandées : `create_database` → `wait_database` (`READY`) ; `create_addon` → `wait_addon` (`RUNNING`) ; `create_workload` (`create_workload_from_github` : conteneur, lien, premier build) → `wait_build` (`SUCCEEDED` ; `FAILED`/`CANCELLED` = échec, avec la cause `failure`) ; `attach_database` ; `attach_addon` ; `add_domain` (le détail porte le premier enregistrement DNS à poser) ; `wait_running` (`RUNNING` ; `FAILED`, `CRASHLOOP`, `IMAGE_PULL_FAILED`, `CONFIG_ERROR`, `OOM_KILLED`, `EVICTED`, `STOPPED`… = échec). Identifiants pris des réponses précédentes, mémorisés dans l'état (`resources`, et le domaine dans l'enregistrement).
- **Événement SSE `action`** : la `View` habituelle (`tool: propose_launch_plan`, `risk: medium`, `target`: nom du conteneur) avec `plan {title, repository, branch, steps: [{key, kind, params}]}`.
- **Validation** (`POST /copilot/actions/:token/approve`) : carte réservée, état initial `running` dans Redis (`betorun:copilot:plan:<jeton>`, 24 h), plan joué dans une goroutine détachée (`context.WithoutCancel`, 30 min au plus) ; réponse immédiate `200 {status: "running", plan: PlanState}`. Note dans la conversation au début et à la fin (statut, identifiants créés, enregistrements DNS).
- **Exécution** ([service/planrun.go](../../internal/modules/copilot/service/planrun.go)) : chaque écriture par `cat.Execute` avec un jeton délégué `write` **neuf de 2 min** (une étape = un jeton), même IP et langue que la validation, clé d'idempotence `<jeton du plan>:<clé d'étape>` ; les attentes relisent avec des jetons `read` (intervalle 3 s, `PlanTimings` injectable ; build 20 min, autres attentes 5 min). Une relecture 4xx arrête l'attente, une erreur 5xx/429 est relue. Un échec arrête le plan (étapes suivantes `skipped`, message de l'API), jamais de nouvelle tentative.
- **Battement** : `heartbeat_at` mis à jour à chaque relecture ; un plan `running` sans battement depuis plus de 2 min est rendu `interrupted` à la lecture (API redémarrée).
- **Arrêt** : `POST /copilot/plans/:token/pause` pose un drapeau Redis ; le plan finit l'étape d'écriture en cours puis passe `paused` (une attente en cours s'arrête aussitôt, `skipped`) ; étapes restantes `skipped`. Pas de reprise dans ce lot.
- **PlanState** : `{token, status: running|done|failed|paused|interrupted, title, started_at, ended_at?, steps: [{key, kind, params, status: pending|running|done|failed|skipped, message?, started_at?, ended_at?, dns? {type, name, value}}], resources {workload_id?, build_id?, database_id?, addon_id?}}`. Métrique `kubemanager_copilot_plans_total{outcome}`.

## Endpoints HTTP

Préfixe `/kmanager/v1`. Groupe interne (hors documentation publique), jamais des outils eux-mêmes.

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/copilot/status` | `Authenticated()` | `{enabled}` : modèle configuré et outils construits |
| POST | `/copilot/chat` | `Authenticated()` (personne connectée) | Message → flux SSE |
| DELETE | `/copilot/conversations/:id` | `Authenticated()` | Oublier une conversation |
| POST | `/copilot/actions/:token/approve` | `Authenticated()` (personne connectée) | Valider une carte |
| POST | `/copilot/actions/:token/reject` | `Authenticated()` (personne connectée) | Refuser une carte |
| GET | `/copilot/plans/:token` | `Authenticated()` (personne connectée, propriétaire ; sinon `404 PLAN_NOT_FOUND`) | État d'un plan « mettre en ligne » |
| POST | `/copilot/plans/:token/pause` | `Authenticated()` (personne connectée, propriétaire) | Arrêter le plan après l'étape en cours (`202`, PlanState) |

Une personne connectée = jeton de session avec utilisateur interne et session (`COPILOT_PERSON_ONLY` sinon : pas de compte de service, pas de jeton délégué).

## Flux SSE (`POST /copilot/chat`)

Corps : `message` (1 à 4000 caractères), `conversation_id` (UUID, sinon un nouveau), `page` (`path`, `project_id`, `workload_id`, `build_id`, `site_id`). Du contexte de page, seuls des UUID bien formés et un chemin de console validé entrent dans le prompt.

Avant le flux, réponses JSON ordinaires : `503 COPILOT_DISABLED`, `400 INVALID_REQUEST`, `429 COPILOT_RATE_LIMITED` (12 messages par personne et par minute, Redis ; Redis injoignable = refus).

Événements : `meta {conversation_id}` · `delta {text}` · `tool {id, name, status: running|ok|failed}` · `action {carte}` · `revising {}` · `replace {answer}` (réponse vérifiée, lot A3) · `done {answer, stop, tool_calls}` · `error {code, message}` avec `COPILOT_BUDGET`, `COPILOT_TOO_LONG`, `COPILOT_CANCELED` ou `COPILOT_UNAVAILABLE`. En-têtes `Cache-Control: no-cache, no-transform` et `X-Accel-Buffering: no`.

Un résultat d'outil est borné à 12 Kio pour le modèle (fin gardée pour les journaux).

## Conversations

Redis, clé `betorun:copilot:conv:<utilisateur>:<conversation>` : une conversation n'est lisible que par son auteur. Seul le texte est gardé (questions, réponses finales, notes des cartes), 20 messages au plus, 8 Kio par message, oublié après 24 h d'inactivité. Les appels d'outils et leurs résultats ne sont pas conservés. Historique indisponible : le Copilot répond quand même, sans mémoire.

## Configuration

Clés `llm.*` ([config.go](../../internal/core/configs/config.go)), réglables par `KUBEMANAGER_LLM_*` :

- `llm.enabled` : faux par défaut ; faux = Copilot désactivé, `status` renvoie `enabled: false`.
- `llm.primary.*` (obligatoire si activé) et `llm.fallback.*` (facultatif, `base_url` vide = absent) : `name`, `base_url` (http/https), `api_key` (secret, par l'environnement), `model` (obligatoire), `timeout` (120 s par défaut), `max_tokens_field` (`max_tokens` ou `max_completion_tokens`), `extra_body_json`, `input_micro_usd_per_mtok`, `output_micro_usd_per_mtok`.
- `llm.daily_budget_micro_usd` : plafond journalier (UTC), **obligatoire > 0** ; le Copilot l'applique par personne (`user:<id>`), pas encore par organisation.
- `llm.breaker_failures` (3) et `llm.breaker_cooldown` (30 s).

Le Copilot exige aussi le STS et Redis ; sans eux il reste désactivé. Métriques : `kubemanager_copilot_turns_total`, `tool_calls_total`, `actions_total` et celles de la couche `llm`.

## Pièges

- Ajouter `ForCopilot` sur une route non documentée publiquement désactive tout le Copilot au démarrage (seul un journal d'erreur le signale).
- Un modèle auto-hébergé sans prix configuré n'est pas plafonné (coût nul compté).
- Le commentaire de `ForCopilot` parle encore de « lecture (GET) » : les écritures sont admises via `Write`.
- Le disjoncteur est en mémoire, par instance.

## Fichiers clés

- [copilot.go](../../internal/modules/copilot/copilot.go) — service, `Chat` (SSE), quota, contrôleur.
- [actions.go](../../internal/modules/copilot/actions.go), [proposals.go](../../internal/modules/copilot/service/proposals.go) — cartes, validation, annulation.
- [plans.go](../../internal/modules/copilot/plans.go), [service/launchplan.go](../../internal/modules/copilot/service/launchplan.go), [service/planrun.go](../../internal/modules/copilot/service/planrun.go), [service/planstore.go](../../internal/modules/copilot/service/planstore.go) — plan « mettre en ligne ».
- [tools/catalog.go](../../internal/modules/copilot/tools/catalog.go), [tools/exec.go](../../internal/modules/copilot/tools/exec.go) — construction depuis l'OpenAPI, exécution interne.
- [agent/agent.go](../../internal/modules/copilot/agent/agent.go), [agent/prompt.go](../../internal/modules/copilot/agent/prompt.go) — boucle et prompt.
- [store/store.go](../../internal/modules/copilot/store/store.go) — mémoire des conversations.
- [copilot_wiring.go](../../internal/infrastructure/router/copilot_wiring.go) — `CopilotTool`, `ForCopilot`, routes, activation.
- [llmsetup.go](../../internal/infrastructure/llm/llmsetup/llmsetup.go) — pile `llm` depuis la configuration.
- [delegation.go](../../internal/iam/sts/delegation.go), [middleware delegation.go](../../internal/middleware/security/delegation.go) — jetons délégués et leur garde.
