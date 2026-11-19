# ADR 0003 — Centrifugo Publisher en mode fail-open

- **Status** : accepté
- **Date** : 2026-01-20

## Contexte

KubeManager publie des events (transitions de statut workload, pod events, progression d'une saga) vers Centrifugo, qui broadcast aux frontends connectés via WebSocket.

Le publisher HTTP (`internal/infrastructure/realtime/publish.go`) est appelé depuis :

- Les activities Temporal (`UpdateWorkloadStatus`, `SoftDeleteWorkload`).
- Le service `clusters` (`OnPodEvent`).
- Potentiellement les workflows `projects`.

Si Centrifugo est down, l'appel HTTP échoue. **Que faire ?**

Deux philosophies :

1. **Fail-closed** : remonter l'erreur. La saga Temporal fail, retry 3×, puis termine en erreur.
2. **Fail-open** : logger l'erreur, continuer. La saga réussit, mais le user ne voit pas l'event UI.

## Décision

**Fail-open**. Le publisher ne propage JAMAIS une erreur de `Publish()` au caller.

Implémentation : [activities/db.go:broadcastDeploymentEvent](../../internal/modules/workloads/activities/db.go) et [clusters/service.go:publishPodEvent](../../internal/modules/clusters/service.go) — les erreurs sont loggées avec `Warnw` et droppées.

## Conséquences

### Positives

- **Isolation** : un incident sur le broker WebSocket n'impacte pas les opérations métier (deploy workload, create project, etc.). La plateforme continue à fonctionner en mode dégradé.
- **Pas de cascade failure** : Centrifugo down ne fait pas tomber le worker Temporal. Une saga `DeployWorkloadWorkflow` en plein milieu d'un `helm upgrade` ne se met pas à retry juste parce que le broadcast final a raté.
- **Simplicité** : les activités ne gèrent pas de retry spécifique Centrifugo ; Temporal fait le retry sur les erreurs métier, pas sur les erreurs UX.

### Négatives

- **Perte d'events UI** : si Centrifugo est down 5 minutes, les frontends ne reçoivent rien pendant ce temps → UX "mon deploy a l'air stuck mais en fait il tourne". Le user peut toujours poll `GET /workloads/:id/status` pour avoir l'état.
- **Debug plus difficile** : si on voit pas les events dans l'UI en dev, ça peut être silencieusement une config mismatch Centrifugo. D'où l'importance des dashboards Grafana qui matchent le publisher error rate.

## Alternatives considérées

### Fail-closed

Rejeté. Centrifugo est un composant d'**UX**, pas un système critique de données. Si on fail-close, un incident broker peut retry-looper un worker Temporal (ou pire, empêcher un workload d'être marqué DEPLOYED en DB).

### Queue intermédiaire (Kafka) entre app et Centrifugo

Overkill pour notre échelle actuelle. On ajouterait un système à opérer (Kafka) pour ne pas perdre ~1% d'events UI, sachant que le user peut toujours poll l'API.

Si on atteint un point où chaque event DOIT être délivré (notifications B2B contractuelles, par exemple), on aura ce débat.

### Fire-and-forget sans timeout

Actuellement on a un timeout de 5 s sur le `POST /publish`. Enlever le timeout = l'activity bloque indéfiniment si Centrifugo est hang. Rejeté.

Mieux : passer en **vraiment fire-and-forget** via un channel Go interne (goroutine dédiée qui consomme et appelle `Publish`). À faire si latence devient un problème — pour l'instant le 5 s timeout est acceptable (voir `httpPublisher` dans [publish.go](../../internal/infrastructure/realtime/publish.go)).

## Invariants à préserver

1. **Publisher ne doit jamais bloquer > 5 s** — si ça bloque, le caller (activity) s'y perd.
2. **Publisher ne doit jamais panic** — un `nil map` ou un type non-serializable doit être catch en amont.
3. **Logs structurés** : chaque erreur publish contient `channel` + `error` pour permettre le filtrage Loki.

## Quand revoir cet ADR

- Si un client B2G nous impose un SLA sur les notifications temps réel (ex: "tout event doit être délivré < 5 s, 99.99%").
- Si on remplace Centrifugo par un broker critique (Kafka as WebSocket terminator avec Pusher-like API).

## Références

- Module : [realtime](../modules/realtime.md)
- Publisher : [infrastructure/realtime/publish.go](../../internal/infrastructure/realtime/publish.go)
- Monitoring : alerting `KubeManagerPublisherErrors` (à ajouter dans [PrometheusRule](../../chart/kubemanager-backend/templates/prometheusrule.yaml) si on veut surveiller le taux d'échec).
