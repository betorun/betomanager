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
- une écriture n'est jamais un `GET` ni un `DELETE` (pas de suppression par le Copilot) ;
- une écriture dont la permission est sensible (`Sensitive` dans `internal/iam/permissions.go`, double authentification) est refusée ;
- une écriture déclare un risque `low` ou `medium` ; un `Undo` doit viser une écriture aux mêmes paramètres de chemin.

Un échec de construction désactive le Copilot (erreur journalisée, l'API démarre quand même). S'y ajoute un outil composite, `diagnose_workload` ([diagnose.go](../../internal/modules/copilot/tools/diagnose.go)) : fiche, état en direct, événements et fin des journaux d'un conteneur, chaque morceau passant par sa route.

À ce jour : 35 routes marquées (projets, conteneurs, domaines, variables, sites, builds, bases de données, GitHub), dont 13 écritures : `create_workload`, `add_workload_domain`, `verify_workload_domain`, `start_workload`, `restart_workload`, `build_from_git`, `cancel_build`, `create_database`, `restore_database` (risque `low`) ; `stop_workload`, `rollback_workload`, `update_workload_variables` (sans `secrets`), `attach_database` (risque `medium`). Seul couple inverse : `stop_workload` ↔ `start_workload`.

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

## Endpoints HTTP

Préfixe `/kmanager/v1`. Groupe interne (hors documentation publique), jamais des outils eux-mêmes.

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/copilot/status` | `Authenticated()` | `{enabled}` : modèle configuré et outils construits |
| POST | `/copilot/chat` | `Authenticated()` (personne connectée) | Message → flux SSE |
| DELETE | `/copilot/conversations/:id` | `Authenticated()` | Oublier une conversation |
| POST | `/copilot/actions/:token/approve` | `Authenticated()` (personne connectée) | Valider une carte |
| POST | `/copilot/actions/:token/reject` | `Authenticated()` (personne connectée) | Refuser une carte |

Une personne connectée = jeton de session avec utilisateur interne et session (`COPILOT_PERSON_ONLY` sinon : pas de compte de service, pas de jeton délégué).

## Flux SSE (`POST /copilot/chat`)

Corps : `message` (1 à 4000 caractères), `conversation_id` (UUID, sinon un nouveau), `page` (`path`, `project_id`, `workload_id`, `build_id`, `site_id`). Du contexte de page, seuls des UUID bien formés et un chemin de console validé entrent dans le prompt.

Avant le flux, réponses JSON ordinaires : `503 COPILOT_DISABLED`, `400 INVALID_REQUEST`, `429 COPILOT_RATE_LIMITED` (12 messages par personne et par minute, Redis ; Redis injoignable = refus).

Événements : `meta {conversation_id}` · `delta {text}` · `tool {id, name, status: running|ok|failed}` · `action {carte}` · `done {answer, stop, tool_calls}` · `error {code, message}` avec `COPILOT_BUDGET`, `COPILOT_TOO_LONG`, `COPILOT_CANCELED` ou `COPILOT_UNAVAILABLE`. En-têtes `Cache-Control: no-cache, no-transform` et `X-Accel-Buffering: no`.

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
- [actions.go](../../internal/modules/copilot/actions.go), [proposals.go](../../internal/modules/copilot/proposals.go) — cartes, validation, annulation.
- [tools/catalog.go](../../internal/modules/copilot/tools/catalog.go), [tools/exec.go](../../internal/modules/copilot/tools/exec.go) — construction depuis l'OpenAPI, exécution interne.
- [agent/agent.go](../../internal/modules/copilot/agent/agent.go), [agent/prompt.go](../../internal/modules/copilot/agent/prompt.go) — boucle et prompt.
- [store/store.go](../../internal/modules/copilot/store/store.go) — mémoire des conversations.
- [copilot_wiring.go](../../internal/infrastructure/router/copilot_wiring.go) — `CopilotTool`, `ForCopilot`, routes, activation.
- [llmsetup.go](../../internal/infrastructure/llm/llmsetup/llmsetup.go) — pile `llm` depuis la configuration.
- [delegation.go](../../internal/iam/sts/delegation.go), [middleware delegation.go](../../internal/middleware/security/delegation.go) — jetons délégués et leur garde.
