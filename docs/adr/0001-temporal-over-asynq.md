# ADR 0001 — Temporal plutôt qu'Asynq pour l'async

- **Status** : accepté
- **Date** : 2025-11-15
- **Dernière revue** : 2026-04-10

## Contexte

KubeManager doit exécuter des opérations qui :

- Touchent **plusieurs systèmes** (DB + Keycloak + K8s + Helm + Centrifugo).
- Peuvent durer **plusieurs minutes** (`helm upgrade --wait` sur une app lente peut prendre 60-90 s).
- Nécessitent un **retry automatique** avec backoff en cas d'erreur transitoire.
- Demandent une **compensation** en cas d'échec partiel (rollback du state déjà posé).

Notre stack existante chez `gophercart` utilise **Asynq** (Redis-backed task queue) pour tout l'async. C'est simple, rapide, mais limité :

- Pas de workflow orchestré (juste des tasks indépendantes).
- Pas de compensation / saga pattern built-in.
- Pas de state durable — si le worker crash, l'état d'avancement est perdu.
- L'historique d'exécution est minimal (pour debug, il faut grep les logs).

## Décision

Utiliser **[Temporal](https://temporal.io)** pour tout l'asynchrone chez kubemanager :

- Workflows = orchestrateurs durables avec state persisté.
- Activities = unités de travail retry-ables, avec idempotence enveloppée.
- Saga pattern supporté nativement (via compensation explicite dans le workflow).
- UI Temporal (`temporal-ui.kubemanager`) permet de debugger un workflow en replay.

Temporal tourne dans l'infra comme un service à part (server + Postgres dédié).

## Conséquences

### Positives

- **Durabilité** : un workflow survit à un restart du worker. L'état est rejoué depuis le log d'événements Temporal.
- **Saga built-in** : chaque workflow déclare sa séquence + compensation. Pas besoin de réinventer l'orchestration en Go.
- **Retry policy déclarative** : `RetryPolicy{MaximumAttempts: 3}` par activity, avec backoff exponentiel.
- **Observabilité** : Temporal UI affiche tout (arbre d'activities, inputs/outputs, erreurs). SDK v1.38+ émet des métriques Prometheus (`temporal_workflow_*`).
- **Long-running sans bloquer HTTP** : le handler retourne 202 dès qu'il a submit ; le workflow tourne en arrière-plan.

### Négatives

- **Infra supplémentaire** : Temporal server + son Postgres. Un service de plus à opérer (HA, backup, upgrade).
- **Courbe d'apprentissage** : modèle déterministe des workflows (pas de `time.Now()`, pas de `rand`…) — piège fréquent pour les nouveaux devs.
- **Test un peu plus lourd** : `testsuite` fonctionne bien mais nécessite des fakes avec signatures strictes (`ctx context.Context`).
- **Overhead** : un simple INSERT DB + un broadcast peut se faire hors workflow (trop léger). Il faut savoir quand utiliser Temporal ou pas — voir [temporal-sagas.md](../architecture/temporal-sagas.md).

## Alternatives considérées

### Asynq

Rejeté parce qu'il manque le workflow + compensation. Un `helm install` partiel (secret créé, chart fail à moitié) ne peut pas se rollback proprement en Asynq sans réécrire nous-mêmes l'orchestration.

### Kafka + Saga manuel

Trop lourd. Kafka est excellent pour du streaming/events, mais pour de l'orchestration de workflow c'est sous-optimal — pas de retry policy, pas d'UI de debug.

### Cadence (ancêtre Temporal)

Même modèle que Temporal, mais communauté et maintenance plus faibles depuis le fork. Temporal a la traction.

### "Juste Go goroutines + DB state machine"

C'est ce qu'on ferait pour un MVP d'une semaine. À l'échelle de kubemanager (N sagas par seconde en prod, durée variable, cluster-wide), on réécrit mal Temporal.

## Contraintes opérationnelles

- Temporal server = SPOF → HA multi-replica obligatoire en prod.
- Postgres Temporal = **isolé** de la DB applicative kubemanager — éviter les contentions et les rollbacks croisés.
- Upgrade Temporal nécessite un plan compatibilité (les workflows in-flight peuvent être impactés si des breaking changes dans le SDK).

## Références

- Docs Temporal : <https://docs.temporal.io>
- Notre doc : [architecture/temporal-sagas.md](../architecture/temporal-sagas.md)
- Exemples : [workloads/workflows/](../../internal/modules/workloads/workflows/), [projects/workflows/](../../internal/modules/projects/workflows/)
