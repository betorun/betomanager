# ADR 0015 — Stockage objet (S3) par projet

Statut : accepté (2026-11-24) ; lots 0 à 3 écrits le 2026-11-24 ([doc du module](../modules/storages3.md)).

## Contexte

La landing annonce un **stockage objet** « à venir » : un espace par projet, joignable depuis les conteneurs, avec l'API S3 habituelle, des liens publics ou privés à durée limitée, l'accès posé en variable secrète et un paiement à l'usage. Aujourd'hui, les applications des clients n'ont nulle part où garder les fichiers reçus ou produits (images envoyées, documents, exports) : le disque d'un conteneur est lié à une seule réplique et disparaît avec lui.

La plateforme a déjà un S3 : **SeaweedFS 4.50** sur le serveur 1 (compose, volume `s3data`, nom public `s3.<domaine>`). Il porte les sites statiques (`betorun-sites`, identité `edge` en lecture seule) et, en dev, les sauvegardes des bases (`betorun-db-backups`, identité limitée à ce bucket). Cette version offre ce dont le produit a besoin :

- une **API IAM intégrée**, compatible AWS (`CreateUser`, `CreateAccessKey`, `PutUserPolicy`…), sur le port S3 (`-iam`, en lecture seule par défaut : `-iam.readOnly=false` pour l'écriture) ;
- des **quotas par bucket** (`s3.bucket.quota`, et `s3.bucket.quota.enforce`, qui passe en lecture seule un bucket qui dépasse) ;
- l'**accès anonyme par bucket** (`s3.anonymous.set`) et les URL signées SigV4 (liens privés à durée limitée).

Contraintes héritées : l'IAM doit être solide dès maintenant (voir [iam.md](../architecture/iam.md)) ; aucun outil n'est nommé côté client (on dit « stockage », jamais SeaweedFS) ; les prix viennent de la grille, jamais du code ; la suppression d'un projet passe par le délai de grâce de 12 h.

## Décision

1. **Un module `storage`, des buckets par projet.** Un bucket appartient à un projet. Le client lui donne un nom lisible, unique dans le projet (même règle que les bases : `^[a-z][a-z0-9-]{1,28}[a-z0-9]$`). Le nom S3 réel est **généré** (`bt-<12 caractères aléatoires>`), parce que l'espace des noms S3 est global : deux clients peuvent ainsi avoir chacun un bucket `uploads`. Le nom réel est celui que l'application utilise (`S3_BUCKET`). Limite de 5 buckets par projet pour commencer.

2. **Une clé par usage, jamais de clé partagée.** Chaque accès est une identité S3 propre, limitée à **un seul bucket** (actions `Read`, `Write`, `List`, `Tagging` sur ce bucket, rien d'autre) :
   - **branchement sur un conteneur** : une clé par couple (bucket, conteneur), posée en variables secrètes (`S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`), puis le conteneur est redéployé. Débrancher supprime la clé ;
   - **clé manuelle** (outils locaux, CI) : créée depuis la console ou la CLI, en lecture seule ou en lecture-écriture. Le secret n'est montré **qu'une fois** ; l'API ne le garde pas en clair.
   Une clé se révoque seule, sans toucher aux autres. La rotation crée une nouvelle clé, redéploie le conteneur, puis supprime l'ancienne.

3. **Le service parle à une interface, pas à SeaweedFS.** `service.Backend` : créer et supprimer un bucket, poser un quota, créer et supprimer une identité limitée à un bucket, lire l'espace utilisé, ouvrir ou fermer la lecture publique. L'implémentation SeaweedFS vit dans `repository` :
   - identités et clés par l'**API IAM** (protocole AWS, signé SigV4), avec une identité d'administration dédiée au module (pas celle des sites) ;
   - quota appliqué par la politique de chaque clé (le quota natif de la SeaweedFS n'est pas utilisé : l'état « quota dépassé » doit laisser supprimer) ; espace utilisé par la liste des objets ; lecture publique par une politique de bucket S3 standard (`s3:GetObject`, `Principal: *`).
   Un autre S3 (Ceph RGW, MinIO, un fournisseur externe) pourra s'y brancher sans toucher au reste. Chaque bucket garde son **emplacement** (`backend`, par défaut l'unique S3 de la plateforme) pour accueillir plus tard un S3 par région.

4. **Le stockage des clients est séparé des données de la plateforme.** Les buckets clients et ceux de la plateforme (sites, sauvegardes) ne partagent aucune identité. L'identité d'administration du module ne peut gérer que les buckets `bt-*` et les identités qu'elle a créées. Les sauvegardes des bases de production restent sur un S3 **d'un autre fournisseur** (déjà prévu : `DATABASES_BACKUP_*`).

5. **Quota et espace utilisé.** Chaque bucket reçoit un quota (taille maximale choisie à la création, modifiable) ; dépassé, ses clés en écriture ne peuvent plus que **lire et supprimer** (une clé en lecture seule le reste ; l'envoi depuis la console est refusé), et une alerte part aux propriétaires et éditeurs à 80 % puis au quota atteint, comme pour les disques. L'espace utilisé est relevé toutes les 15 minutes et affiché (console, CLI `storage list`), à la manière de `diskusage`.

6. **Liens publics et privés.**
   - **Privé** (par défaut) : rien n'est lisible sans clé. L'application crée elle-même ses liens à durée limitée avec sa bibliothèque S3 (URL signée SigV4).
   - **Public** : le client peut ouvrir un bucket en lecture publique (lister refusé, seule la lecture d'un objet dont on connaît le nom est permise). Adresse : `https://s3.<domaine>/<bucket>/<objet>`. Action sensible (double authentification), affichée en évidence sur la carte du bucket.

7. **IAM.** Nouvelles permissions :
   - `storage.buckets.list`, `storage.buckets.get` (lecteur) ;
   - `storage.objects.list` (lecteur) ; `storage.objects.get`, `storage.objects.create` (développeur) ;
   - `storage.buckets.create`, `storage.buckets.update` (quota), `storage.buckets.connect` (brancher, clés, rotation), `storage.objects.delete`, `storage.objects.share` (liens signés) (éditeur) ;
   - `storage.buckets.delete` et `storage.buckets.setPublic` (propriétaire, **sensibles**) ;
   - rôles prédéfinis `roles/storage.admin` et `roles/storage.objectUser`.
   La console parcourt les fichiers **par l'API** (qui vérifie les droits du projet puis lit avec l'identité du module) ; aucune clé client n'arrive dans le navigateur.

8. **Cycle de vie.**
   - Supprimer un bucket : il doit être vide, ou on confirme par son nom (double authentification) pour effacer aussi son contenu. Ses identités sont supprimées d'abord.
   - Projet en délai de grâce (12 h) : les identités du projet sont **désactivées** (les applications ne lisent ni n'écrivent plus) et la lecture publique fermée, les données sont gardées ; restauration : tout revient. Purge : contenu, buckets et identités effacés par une activité du workflow de suppression, avant les lignes du projet. La purge d'une organisation suspendue (facturation) efface aussi ses buckets.
   - Rien n'est facturé pendant le délai de grâce (même règle que les conteneurs et les bases).

9. **Facturation.** Compteur `storage` en Go·heure (espace relevé, arrondi au Go supérieur comme les disques : la précision des montants ne permet pas le Mo), prix mensuel au Go dans la grille (`storage_gb_monthly`, 0 tant qu'il n'est pas fixé). Un bucket vide ne coûte rien. Le trafic sortant n'est pas facturé en V1 ; il est mesuré pour décider plus tard.

10. **Ce que voit le client** : une section **Stockage** (dans Services, à côté des bases), une carte par bucket (nom, espace utilisé sur quota, public ou privé, conteneurs branchés, clés), un explorateur de fichiers (dossiers, envoi par glisser-déposer jusqu'à 100 Mo, téléchargement, suppression, nouveau dossier, lien de partage de 1 h à 7 jours, adresse publique), et la CLI `betorun storage …` (`ls`, `cp`, `rm`, `link`, `public`, `keys rotate`). Le Copilot peut lister, créer un bucket et le brancher (les actions sensibles restent dans la console).

## Découpage

| Lot | Contenu |
|---|---|
| 0 | Essai sur la SeaweedFS de dev : API IAM en écriture (`-iam.readOnly=false`), identité d'administration limitée aux buckets `bt-*`, quota appliqué en lecture seule, accès anonyme en lecture seule sans liste, URL signée. Confirme le point 3 avant d'écrire le module. |
| 1 | Module `storage` : buckets, quota, identités, branchement sur un conteneur (variables secrètes et redéploiement), clés manuelles, espace utilisé ; API, CLI, console (carte, création, branchement), traductions, Copilot, doc. |
| 2 | Explorateur de fichiers dans la console, accès public, alertes de quota, rotation des clés. |
| 3 | Compteur `storage` dans la facturation, délai de grâce (désactivation, purge), landing : retrait de « À venir ». |
| Plus tard | S3 par région (dans les clusters), versions d'objets, règles d'expiration, domaine personnalisé sur un bucket public (par static-edge). |

## Options écartées

- **Une seule clé par projet** : plus simple, mais une fuite expose tous les buckets du projet et ne se révoque pas sans casser toutes les applications.
- **Noms S3 choisis par le client** : l'espace des noms est global ; les collisions entre clients seraient inévitables et révéleraient l'existence d'autres buckets.
- **Un MinIO dédié** : un service de plus à exploiter, sans gain fonctionnel sur ce que SeaweedFS fait déjà ; l'interface du point 3 garde la porte ouverte.
- **Volume persistant partagé (RWX) au lieu de S3** : pas d'accès hors du cluster, pas de liens, et la plupart des bibliothèques web attendent du S3.
- **Clés gérées par `weed shell` depuis l'API** : outil d'exploitation, sans contrat stable ; l'API IAM est un protocole connu et remplaçable.

## Conséquences

- SeaweedFS de production démarre avec l'IAM en écriture et une nouvelle identité d'administration (secret neuf `S3_STORAGE_SECRET_KEY`) ; Caddy ouvre `s3.<domaine>` aux seuls chemins des buckets clients (`deploy/README.txt`, section 7 quinquies).
- La SeaweedFS n'accepte que des politiques `Allow` : une identité désactivée n'est autorisée que sur une ressource impossible.
- Le volume `s3data` grandit avec les données des clients : il faut suivre le disque du serveur 1, et la sauvegarde de nuit (`deploy/backup.sh`, copie du volume vers un autre fournisseur) prend plus de temps et de place. Au-delà d'un seuil à fixer, le stockage client passe sur un disque ou un serveur à part.
- Les conteneurs joignent le stockage par `https://s3.<domaine>` (serveur 1) : trafic entre les deux serveurs. Acceptable en V1 ; un S3 dans chaque cluster (« Plus tard ») le supprime.
- Le module `storage` rejoint la purge des projets, la facturation, l'IAM (rôles, traductions des permissions) et le Copilot (une décision par route, test de couverture).
