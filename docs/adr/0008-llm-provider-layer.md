# ADR 0008 — Couche d'accès aux modèles de langage

- **Status** : proposé
- **Date** : 2026-11-18
- **Code** : [internal/infrastructure/llm](../../internal/infrastructure/llm)

## Contexte

Le Copilot betorun (agent conversationnel qui lit l'état d'un projet et agit
après validation) a besoin d'un modèle de langage. On veut :

- pouvoir changer de modèle sans toucher au Copilot : Qwen auto-hébergé sur
  vLLM, Groq, Gemini, OpenAI… ;
- survivre à la panne d'un fournisseur ;
- plafonner la dépense par organisation (un plan gratuit ne doit pas pouvoir
  coûter cher) ;
- ne pas reproduire la fragilité du Copilot SYK. Celui-ci demande au modèle une
  enveloppe JSON en texte libre et l'analyse lui-même : réparations JSON,
  relances, expressions régulières, appels d'outils au nom vide perdus.

Le service d'IA de SYK (syk-ia) n'a pas été retenu. Son API gRPC n'expose pas
l'appel d'outils natif, et il lierait un produit public à l'infrastructure
interne d'un autre produit.

## Décision

1. **Appel d'outils natif, et seulement lui.** Les outils partent en schémas
   JSON (`llm.Tool`), les appels reviennent structurés (`llm.ToolCall`). Le
   texte du modèle n'est jamais analysé pour y chercher des appels.

2. **Un contrat, des adaptateurs.** `llm.Provider` (`Generate`, `Stream` en
   `iter.Seq2`). Premier adaptateur : `openai`, pour toute API compatible
   « Chat Completions ». Il couvre vLLM, Groq, Gemini (point d'accès
   compatible), OpenAI, Mistral, OpenRouter et Ollama. Un adaptateur natif
   (Anthropic, Gemini) s'ajoutera derrière le même contrat, choisi dans
   `llmsetup`.

3. **Sorties vérifiées à la frontière.** Chaque adaptateur passe les appels
   d'outils par `llm.CheckToolCalls` : nom non vide et proposé, arguments en
   objet JSON. Sinon, `KindMalformedOutput`, traité comme une panne du
   fournisseur, ce qui déclenche la bascule. En aval, un `ToolCall` est
   toujours sûr à router.

4. **Le reste en décorateurs**, une responsabilité chacun, assemblés par
   `llmsetup` :

   ```
   Instrument( Chain( Breaker(Budget(primary)), Breaker(Budget(fallback)) ) )
   ```

   - **Budget** : réserve le coût maximal avant l'appel (entrée estimée par
     défaut à 3 octets par jeton, plus `MaxOutputTokens`, obligatoire), puis
     règle le coût réel. Le trop-réservé est rendu, contrairement à syk-ia.
     Plafond par organisation et par jour UTC, dans Redis, par script Lua
     atomique, en fail-closed (même choix que l'ADR 0006). Un flux coupé garde
     toute sa réservation : on ne sait pas ce qu'il a coûté.
   - **Breaker** : après N pannes de suite (3 par défaut), le fournisseur est
     mis de côté pendant un délai (30 s par défaut), puis un seul appel d'essai
     passe. Seules les pannes du fournisseur comptent : une requête trop
     longue, un plafond atteint ou un appelant parti ne l'ouvrent pas.
   - **Chain** : bascule sur `unavailable`, `timeout`, `rate_limited`, `auth`,
     `malformed_output` et `circuit_open`. Jamais sur une erreur de la requête
     ou du budget, ni quand l'appelant a abandonné. En flux, la bascule n'a
     lieu qu'avant le premier événement transmis : on ne recolle jamais deux
     réponses.
   - **Instrument** : `kubemanager_llm_calls_total`, `call_duration_seconds`,
     `time_to_first_event_seconds` et `tokens_total`. Labels bornés seulement :
     fournisseur, mode, résultat.

5. **Configuration en deux emplacements fixes** (`llm.primary`,
   `llm.fallback`) plutôt qu'une liste, pour que chaque champ reste réglable par
   variable d'environnement (`KUBEMANAGER_LLM_PRIMARY_API_KEY`…). Un modèle
   auto-hébergé reçoit un prix interne, sinon rien n'est plafonné.

6. **Ce qui ne part jamais chez le fournisseur** : `Request.Caller`
   (organisation, utilisateur, tour), qui sert au budget et aux journaux ; la
   clé d'API, jamais présente dans une erreur ; le corps d'une réponse en
   erreur, tronqué à 300 caractères. Garder les valeurs des secrets des
   conteneurs hors du prompt est la responsabilité du Copilot.

## Conséquences

### Positives

- Le Copilot ne connaît qu'un contrat. Changer de modèle est une affaire de
  configuration.
- Les erreurs sont typées (`llm.Kind`) : le Copilot sait dire « plafond
  atteint », « fournisseurs indisponibles » ou « demande trop longue », au lieu
  d'une erreur générique.
- Tout se teste sans réseau : `llmtest` fournit un fournisseur scripté, et
  l'adaptateur est testé contre un faux serveur (flux fragmenté, CRLF, trame
  d'usage, erreurs 4xx/5xx, délai).

### Négatives

- Le disjoncteur est en mémoire, par instance : chaque instance redécouvre la
  panne (au plus N appels perdus par instance).
- L'estimation de l'entrée (3 octets par jeton) surréserve. C'est sans gravité,
  le surplus étant rendu, mais un plafond très bas peut refuser un appel qui
  serait passé.
- L'adaptateur compatible OpenAI gomme les spécificités des fournisseurs (cache
  de prompt Anthropic, sorties structurées strictes). Ce sera la raison d'un
  adaptateur natif le jour où il apportera plus que ça ne coûte.

## Alternatives écartées

- **syk-ia derrière** : pas d'appel d'outils natif, couplage entre produits.
- **Bibliothèque tierce multi-fournisseurs** (langchaingo, etc.) : surface
  large, abstractions qui fuient, et l'essentiel (budget, bascule propre en
  flux, vérification des outils) resterait à écrire.
- **LiteLLM en service séparé** : un processus Python de plus à exploiter, pour
  ce que quelques centaines de lignes Go font ici.
