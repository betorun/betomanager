# ADR 0004 — Cluster watcher comme binaire séparé

- **Status** : accepté
- **Date** : 2026-02-05

## Contexte

Kubemanager pilote N clusters K8s distants (K3s ou managés). On a besoin de remonter en temps réel les événements des pods (CrashLoopBackOff, OOMKilled, ImagePullBackOff, …) vers le plan de contrôle central.

Deux options :

- **Push (watcher)** : un binaire dans chaque cluster qui observe les pods (client-go informers) et pousse vers kubemanager via HTTP.
- **Pull (kubemanager interroge)** : kubemanager ouvre une connexion persistante vers l'API Server de chaque cluster et reçoit les events via Watch endpoint.

## Décision

**Watcher en binaire séparé**, déployé dans chaque cluster géré, qui pousse vers kubemanager via HTTP avec une API key `kind=cluster`.

Projet distinct : `betorun/watcher`. Helm chart fourni : `betorun/watcher/chart/`.

## Conséquences

### Positives

- **Résilient au network** : le watcher côté cluster retry localement avec backoff. Un réseau flaky entre les datacenters ne fait pas tomber la télémétrie.
- **Scalable horizontalement** : plus de clusters = plus de watchers indépendants. Kubemanager ne maintient pas N connexions persistantes.
- **Sécurité simplifiée** : kubemanager n'a pas besoin de kubeconfig vers les clusters gérés pour **observer** (uniquement pour déployer via Helm, qui est un flow distinct).
- **Déploiement par cluster** : un watcher peut être version-locked à son cluster (upgrade progressif).
- **Leader election** via Lease K8s — un seul pod watcher actif à la fois, même si on scale à 3 replicas.
- **Batching** : le watcher accumule les events et pousse en batch (`batch_size: 50`, `batch_flush_ms: 1000`). Réduit la charge kubemanager.

### Négatives

- **Deux binaires à maintenir** : kubemanager + watcher. Coordination des releases, compatibilité API.
- **Authentification** : chaque watcher a sa propre API key → gestion de rotation, audit.
- **Observabilité du watcher lui-même** : il tourne côté cluster géré, donc logs/metrics pas forcément accessibles depuis kubemanager. Alternative : forwarder Prometheus/Loki côté cluster géré vers la stack centrale.
- **Latence** : un event K8s prend ~200-500 ms pour arriver à kubemanager (informer → batch → HTTP). Acceptable pour de la surveillance, pas pour du control plane critique.

## Alternatives considérées

### Pull — kubemanager watch l'API Server

Rejeté parce que :

- N connexions persistantes à maintenir. Pour N=100 clusters, c'est 100 goroutines qui consomment de la mémoire et qui gèrent des reconnexions.
- Exige un kubeconfig par cluster dans le kubemanager lui-même, donc un secret stor ou mount que le control plane doit sécuriser.
- Pas de batching natif — chaque event est un tick réseau.
- En cas de downtime kubemanager, les events sont perdus (pas de buffer côté cluster).

### Agent daemon-set sur chaque node (pattern Prometheus Node Exporter)

Trop granulaire. On veut observer les pods, pas les nodes. Un agent node-level aurait une cardinalité x N_nodes × N_pods — inutile.

### Kube state metrics + scrape Prometheus central

Complète mais insuffisant. kube-state-metrics expose les **états**, pas les **événements**. On perd la granularité "pod X a crashed à cette seconde avec cette reason".

### Pilotage via Kubelet's `/events` endpoint

Pas stable, pas documenté comme API publique. Pas envisageable.

## Protocole (Restate-compatible)

Le watcher suit 3 endpoints sur kubemanager :

1. **Enroll** — au boot, annonce `{cluster_id, version}`.
2. **GetConfig** — récupère la config remote (dedup, batch size, reasons actives).
3. **OnPodEvent** — pousse chaque event significatif.

Voir [modules/clusters.md](../modules/clusters.md) pour les détails du protocole.

Nommage "Restate-compatible" — si un jour on migre vers Restate (`restate.dev`) pour orchestrer des microservices distribués, la forme des endpoints colle déjà à leur convention.

## Sécurité

- API key `kind=cluster`, scope `cluster:report` obligatoire.
- NetworkPolicy côté watcher : egress uniquement vers l'URL kubemanager (HTTPS).
- Rotation périodique avec grace period (voir [how-to/rotation-cluster-key.md](../how-to/rotation-cluster-key.md)).

## Quand revoir cet ADR

- Si on passe à < 5 clusters gérés, le tradeoff bascule vers le pull direct (moins de binaires à maintenir).
- Si les latences du watcher deviennent problématiques, on peut passer en **streaming gRPC** (bidirectionnel) plutôt que HTTP batch.

## Références

- Protocole : [modules/clusters.md](../modules/clusters.md)
- Projet watcher : `betorun/watcher`
- Chart : `betorun/watcher/chart/`
- Restate (inspiration naming) : <https://restate.dev>
