# `clusters` — AI Context

## Responsabilité
Enregistrement des clusters K8s distants et ingestion des pod events via un watcher externe (projet `betorun/watcher`).

## Protocole (3 étapes)

1. **Enroll** : le watcher POST `/clusters/ClusterService/Enroll` au démarrage avec `{cluster_id, version}`. Serveur upsert dans la table `clusters`.
2. **GetConfig** : le watcher GET la config remote (dedup, batch size, reasons actives) au boot et périodiquement.
3. **OnPodEvent** : le watcher POST chaque pod event significatif → kubemanager update le workload et publish Centrifugo.

## Invariants critiques
- **API key cluster** : kind="cluster", scope `cluster:report` obligatoire.
- **Label workload** : les pods K8s portent `km-workload-id=<uuid>` pour mapping.
- **Idempotence** : OnPodEvent doit être safe-retry ; le watcher dédup côté client mais le serveur peut recevoir des doublons.
- **Last seen** : `TouchLastSeen()` appelé à chaque onPodEvent pour détection staleness.

## Fichiers clés
- `internal/modules/clusters/hclusters.go` — controller
- `internal/modules/clusters/service.go` — Enroll/GetConfig/OnPodEvent/ListClusters
- `internal/modules/clusters/repository.go` — CRUD table clusters
- `internal/modules/clusters/entity.go` — domain types (Cluster, PodEvent, PodEventAck, RemoteConfigResponse)

## Dépendances
- `auth` (IAPIKeyService) pour valider les clusters keys.
- `workloads` repository pour mapping pod → workload.
- `realtime.Publisher` pour broadcast.

## Règles pour l'assistant
- Ne jamais modifier la table `clusters` en dehors du service Enroll.
- La config remote est source unique de vérité pour le comportement watcher — ne pas dupliquer côté watcher.
- Le mapping `reason K8s → status interne` est hardcodé dans le service ; modifier avec prudence (impact UX dashboard).
