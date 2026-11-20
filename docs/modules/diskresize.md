# Module diskresize — agrandir un disque par copie

Le stockage du cluster (`local-path`) ne sait pas agrandir un disque : un
`helm upgrade` avec une taille plus grande échouait. Le module agrandit le
disque d'un conteneur (et du porteur d'un service) **par copie**, conteneur
arrêté. Code : [internal/modules/diskresize](../../internal/modules/diskresize).

## Déroulé ([resize.go](../../internal/modules/diskresize/resize.go))

| Phase | Ce qui se passe | En cas d'échec |
|---|---|---|
| `stopping` | arrêt du conteneur (`Stop`), attente qu'aucun pod n'utilise `<nom>-pvc` | disque temporaire supprimé, conteneur relancé s'il tournait |
| `copying` | disque temporaire `<nom>-pvc-resize` à la nouvelle taille, copie vérifiée | idem : **disque d'origine intact** |
| `swapping` | `<nom>-pvc` supprimé puis recréé à la nouvelle taille, mêmes étiquettes et annotations Helm (Helm le reprend au déploiement suivant) | opération reprenable |
| `restoring` | recopie vérifiée du disque temporaire vers le nouveau disque | opération reprenable |
| `starting` | nouvelle taille enregistrée et redéploiement (`Update`) ; arrêté de nouveau s'il l'était avant ; disque temporaire supprimé | opération reprenable |

- **Rien n'est supprimé sans copie vérifiée.** Après `swapping`, les données
  sont sur le disque temporaire : une nouvelle demande (même taille, ou taille
  vide) **reprend** à la phase enregistrée. À la reprise, le disque d'origine
  n'est supprimé que s'il n'a pas déjà la nouvelle taille.
- **Copie** ([kube.go](../../internal/modules/diskresize/kube.go)) : Job busybox
  (non root, utilisateur et groupe du conteneur : `run_as_user`/`run_as_group`
  de la spec, sinon 1000/2000 comme le chart), source montée en lecture seule.
  La cible est vidée, `tar | tar`, puis comparaison du nombre d'entrées et de la
  somme des tailles ; `pipefail` : un fichier illisible fait échouer la copie.
- **Origine gardée** : le disque temporaire porte l'annotation
  `betorun.app/resize-origin` (JSON : étiquettes, annotations `meta.helm.sh/*`,
  classe de stockage) ; la recréation l'utilise.
- **Une opération à la fois** par conteneur (verrou en mémoire). Une opération
  « en cours » sans verrou (API redémarrée) est présentée comme interrompue et
  se reprend.
- **Quota** : la nouvelle taille compte dans le plafond de stockage du projet
  (même calcul que la mise à jour d'un conteneur).
- **Ancien chemin fermé** : `PATCH /workloads/:id` avec un autre
  `storage_size` sur un conteneur à disque → `400`, renvoie vers cette route.

## Routes

| Méthode | Chemin | Permission | Rôle |
|---|---|---|---|
| GET | `/workloads/:id/disk` | `workloads.workloads.get` | dernière opération, tailles proposées (vide sans disque) |
| POST | `/workloads/:id/disk` | `workloads.workloads.update` | `{"size":"5Gi"}` → `202` ; taille vide : reprise. Porteur d'un service → `400` |
| GET | `/projects/:id/addons/:addonID/disk` | `addons.instances.get` | idem pour un service |
| POST | `/projects/:id/addons/:addonID/disk` | `addons.instances.update` | tailles du modèle seulement |

Tailles : 1Gi, 2Gi, 5Gi, 10Gi, 20Gi, toujours plus grandes que l'actuelle.
Outil Copilot : `resize_workload_disk`. Table `disk_resizes` (migration 000052).

## Console

Fiche d'un conteneur (onglet Vue d'ensemble, carte Disque) et fiche d'un
service persistant : bouton « Agrandir le disque », choix de la taille, suivi
de l'étape en cours (toutes les 3 s), message d'échec et bouton « Reprendre ».

## Tests

- `resize_test.go` : déroulé complet, conteneur arrêté qui le reste, échec de
  copie (retour en arrière), échec après échange (reprise sans nouvelle
  suppression), reprise après recréation, opération interrompue, refus.
- `kube_e2e_test.go` : vraies étapes Kubernetes dans un espace jetable
  (`pod-security restricted`) ; lancé seulement avec `DISKRESIZE_E2E_NS` et
  `KUBECONFIG`. Passé le 2026-11-20 sur le cluster : aller-retour vérifié par
  md5, étiquettes Helm gardées, fichier illisible → échec propre.
- Pas encore fait : un agrandissement complet d'un vrai conteneur par l'API
  (arrêt, redéploiement Helm sur le disque recréé).
