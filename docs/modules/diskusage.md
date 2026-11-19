# Module `diskusage`

> Usage réel des disques des projets. Le stockage du cluster (`local-path`) ne plafonne pas la taille demandée : un disque de 1 Gi peut grossir jusqu'à remplir le serveur, et donc gêner les autres clients. Ce module mesure chaque disque et applique une règle de dépassement.

## Règle

| Usage (taille payée) | Effet |
|----------------------|-------|
| ≥ 80 % | E-mail aux propriétaires et éditeurs du projet (une fois par 24 h au plus) |
| ≥ 100 % | E-mail « disque plein » (une fois par 24 h au plus) |
| ≥ 120 % | **Conteneur** : arrêté (mis à 0, données gardées), e-mail « arrêté : disque plein ». Relancé par le client, il a 6 h (`StopGrace`) pour faire de la place avant un nouvel arrêt. **Base PostgreSQL** : e-mail seulement, jamais d'arrêt automatique (pas encore de relance d'une base dans la console). |

Un e-mail part quand un seuil est franchi vers le haut (redescendre puis remonter en renvoie un). Un conteneur déjà arrêté par le client n'est pas touché : il reçoit seulement l'alerte. Un conteneur recréé sous le même nom repart de zéro.

Les services additionnels (Redis via Valkey…) suivent la règle des conteneurs : leur porteur est un conteneur.

## Mesure

Toutes les heures (`DefaultEvery`, premier passage 2 min après le démarrage du worker), pour chaque projet `READY` :

1. Inventaire des PVC liés de l'espace : disque d'un conteneur (`<release>-pvc`, étiquette `app.kubernetes.io/instance`) ou données d'une base CloudNativePG (`cnpg.io/cluster=db-<nom>`, rôle `PG_DATA`). Les autres disques sont ignorés.
2. Un **Job** `disk-usage-…` par disque, dans l'espace du projet : `busybox`, `du -sk` sur le disque monté **en lecture seule**, avec l'utilisateur du pod qui utilise le disque (jamais root ; pas de `fsGroup`, qui changerait les droits des fichiers), système de fichiers en lecture seule, aucune capacité, 32 Mi de mémoire au plus, 2 min au plus. Le résultat est lu dans le journal du pod, puis le Job est supprimé.
3. Dernière mesure gardée dans `disk_usage` (migration 000047, une ligne par disque ; les disques disparus sont retirés).

Aucun droit nouveau sur le cluster : la plateforme crée déjà des Jobs et lit les journaux des pods dans les espaces des projets.

## Route

| Méthode | Chemin | Permission | Description |
|---------|--------|-----------|-------------|
| `GET` | `/projects/:id/disk-usage` | `resourcemanager.projects.get` | `{disks: [{kind, target_id, target_name, capacity_bytes, used_bytes, level, measured_at, stopped_at?}]}` ; vide avant le premier passage |

## Console

- Conteneur, onglet « Vue d'ensemble », carte « Disque persistant » : jauge, date de mesure, avertissement à 80 / 100 %, « Conteneur arrêté : disque plein » après un arrêt.
- Page « Services » : usage dans la colonne des ressources (`0,4 Go / 1 Go`) et jauge dans le détail de chaque base et service.

## Code

- [usage.go](../../internal/modules/diskusage/usage.go) : seuils, modèle.
- [measure.go](../../internal/modules/diskusage/measure.go) : inventaire et Job de mesure.
- [service.go](../../internal/modules/diskusage/service.go) : passages, règle, stockage.
- Alertes : [alerts/disk_usage.go](../../internal/modules/alerts/disk_usage.go) (types `disk.80`, `disk.100`, `disk.120`).
- Câblage : [router/diskusage_wiring.go](../../internal/infrastructure/router/diskusage_wiring.go).
