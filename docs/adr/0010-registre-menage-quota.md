# ADR 0010 — Ménage et quota du registre de la plateforme

Date : 2026-11-18 · Statut : implémenté

## Décisions

- Taille : à la fin d'un build réussi (activité `Finish`), l'API lit le manifeste
  au registre (index BuildKit compris, couche commune comptée une fois) et
  enregistre `builds.image_size` (migration 000025).
- Ménage, au même moment, au mieux : par dépôt `<projet>/<nom>`, on garde les
  `keep_images` dernières images (défaut 5) et toutes celles d'un conteneur du
  projet. Les autres : `DELETE /v2/<dépôt>/manifests/<digest>` avec un jeton que
  l'API se délivre (sujet `platform`, action `delete`), puis `image_pruned_at`.
  Un digest encore porté par une image gardée n'est pas supprimé (la suppression
  vaut pour toutes ses étiquettes). Les builds d'un autre hôte (ancien registre)
  sont ignorés.
- Quota : `project_image_quota` (défaut 5Gi, "0" = sans). À la soumission d'un
  build : si l'usage (somme des tailles, un digest par dépôt) atteint le quota,
  le projet est d'abord compacté (dernière image + images en ligne par dépôt) ;
  refus `IMAGE_QUOTA` (409) seulement si ça ne suffit pas. Un push GitHub refusé
  laisse un build FAILED `IMAGE_QUOTA`.
- Disque : libéré par `deploy/registry/gc.sh` (garbage-collect **sans**
  `--delete-untagged`, registre arrêté le temps du ménage), à planifier la nuit.
- Pas de workflow modifié : historiques Temporal en cours inchangés.

## Réglages

`builds.registry_api_url` (registre vu par l'API ; défaut https://<registry_host>),
`builds.keep_images`, `builds.project_image_quota`.

## Vérifié

Deux builds réels (crccheck/docker-hello-world) avec keep=1 : taille enregistrée
(2,2 Mo), le plus ancien supprimé (DELETE 202 au registre 3.0.0), dry-run du
garbage-collect ne visant que ses couches.
