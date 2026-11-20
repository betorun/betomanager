# Module `addons`

> Services additionnels d'un projet (ADR [0013](../adr/0013-services-additionnels.md)) : logiciels du catalogue (Redis via Valkey, MariaDB, Meilisearch) installés, surveillés et mis à jour par betorun, joignables seulement depuis les conteneurs du projet. Les bases PostgreSQL restent dans le module [databases](databases.md) ; la console montre les deux sur la page « Services ».

## Modèle

Un service = un **conteneur porteur** ordinaire de sorte `service` (Service Kubernetes TCP sur le vrai port, une instance, stratégie `Recreate`, disque en mode persistant) + une ligne `project_addons` (migration 000044). Mêmes quotas et même facture qu'un conteneur.

- **Catalogue** fermé, dans le code ([catalog.go](../../internal/modules/addons/domain/catalog.go)), images figées :
  - `redis` → `valkey/valkey` (9.1.4 recommandée, 9.0.8, 8.1.12). Titre « Redis (via Valkey) », logo de Valkey, jamais celui de Redis (marque de Redis Ltd). Usages `persistent` (défaut ; `noeviction`, AOF sur disque ; files de tâches, sessions) ou `cache` (`allkeys-lru`, sans disque) ; `maxmemory` ≈ 80 % de la limite.
  - `mariadb` → `mariadb` (11.8.9 recommandée, 11.4.13 ; LTS), compatible MySQL. Utilisateur `999`, disque `/var/lib/mysql`, base et utilisateur `app`, root joignable seulement par le socket local (`MARIADB_ROOT_HOST=localhost`), `MARIADB_AUTO_UPGRADE=1` (montée de version), cache InnoDB = moitié de la limite, `performance_schema` coupé. Adresse `mysql://app:<mot de passe>@<nom>:3306/app` dans `DATABASE_URL`. Persistant seulement. **Sauvegardé chaque nuit** (voir plus bas).
  - `meilisearch` → `getmeili/meilisearch` (v1.57.0 seule : passer d'une version mineure à l'autre demande une migration des données, pas encore automatisée). Utilisateur `1000`, disque `/meili_data`, `MEILI_ENV=production`, indexation ≈ 60 % de la limite sur 2 fils. Le mot de passe est la clé maîtresse : les conteneurs branchés reçoivent l'adresse `http://<nom>:7700` dans `MEILISEARCH_URL` et la clé dans `MEILISEARCH_KEY` (variable choisie sans `_URL` + `_KEY`). Persistant seulement.
- Les deux nouveaux modèles ont été essayés sous Docker avec les mêmes réglages (non root, capacités retirées) : démarrage, sonde, authentification, copie et restauration.
- **Tailles** : `mini` (256 Mi), `small` (512 Mi, défaut), `medium` (1 Gi). `maxmemory` ≈ 80 % de la limite, la mémoire réservée vaut la limite.
- **Disques** : 1, 2 ou 5 Gi (10 Gi aussi pour MariaDB et Meilisearch) en mode persistant, taille fixe (le stockage `local-path` ne sait pas agrandir ; voir l'ADR).
- **Mot de passe** : généré par l'API, posé comme variable SECRÈTE `ADDON_PASSWORD` du porteur, et sous les noms que lit l'image (`MARIADB_PASSWORD`, `MARIADB_ROOT_PASSWORD`, `MEILI_MASTER_KEY` : `PasswordAliases`). Jamais en clair dans les arguments (Valkey : `$(ADDON_PASSWORD)`). Il n'est jamais renvoyé : seuls les conteneurs branchés le reçoivent, dans l'adresse de connexion.
- **Plafond** : `projects.max_addons` (10 par défaut), réglable par le staff : `PATCH /admin/projects/:id/max-addons` (0 à 100, route interne).

## Routes

Préfixe `/projects/:id/addons`.

| Méthode | Chemin | Permission | Description |
|---------|--------|-----------|-------------|
| `GET` | `/templates` | `addons.instances.list` | Catalogue : modèles, tailles et services annoncés (`upcoming`) |
| `GET` | `` | `addons.instances.list` | Services du projet et plafond (`{addons, max}`) |
| `POST` | `` | `addons.instances.create` (éditeur) | `{template, name, version?, mode?, size?, disk?}` ; le nom est aussi l'adresse (`nom:port`) |
| `GET` | `/:addonID` | `addons.instances.get` | Détail et état (celui du porteur) |
| `POST` | `/:addonID/upgrade` | `addons.instances.update` (éditeur) | `{version?}` : plus récente seulement, jamais de retour en arrière |
| `POST` | `/:addonID/attach` | `addons.instances.connect` (développeur) + droit sur les variables du conteneur visé | `{workload_id, variable?}` : adresse posée en variable secrète (`REDIS_URL` par défaut), conteneur redéployé |
| `DELETE` | `/:addonID` | `addons.instances.delete` (éditeur, sensible : double authentification) | Supprime le porteur et son disque ; renvoie `still_attached` (les conteneurs branchés gardent leur variable) |

| `GET` | `/:addonID/backups` | `addons.instances.get` | Sauvegardes (`{backups, kept}`), les plus récentes d'abord |
| `POST` | `/:addonID/backups` | `addons.instances.update` (éditeur) | Copie tout de suite, en arrière-plan → 202 (ligne `running`) |
| `POST` | `/:addonID/backups/:backupID/restore` | `addons.instances.restore` (éditeur, sensible) | `{confirm_name}` ; copie de l'état actuel puis rechargement (synchrone) |

« Me prévenir » (personnel, hors projet, tout utilisateur connecté) : `GET /addons/waitlist`, `PUT|DELETE /addons/waitlist/:template` (services annoncés seulement ; aucun aujourd'hui : MariaDB et Meilisearch sont ouverts). Table `addon_waitlist` (migration 000046), langue de la demande gardée. Quand un service passe dans le catalogue, le worker envoie un seul e-mail par demande (au démarrage puis toutes les heures ; un échec d'envoi est retenté).

## Sauvegardes (MariaDB)

- **Quand** : le worker passe toutes les heures (`RunBackups`) ; chaque service sauvegardable d'un projet actif sans copie réussie depuis 23 h en reçoit une (échec retenté après 3 h). Aussi « Sauvegarder maintenant » dans la console.
- **Comment** : un Job dans l'espace du projet ([backup_job.go](../../internal/modules/addons/backup_job.go)). Amorce dans l'image du service, utilisateur du service : `mariadb-dump --single-transaction --routines --triggers --events --add-drop-database --databases app | gzip`, mot de passe lu dans le secret du porteur (`<nom>-env`, clé `ADDON_PASSWORD`) en `MYSQL_PWD`, jamais en argument ; puis curl envoie la copie à `PUT /addons/backups/:taskID/upload` (URL signée 30 min, acceptée une fois et seulement pendant la tâche). Le pod porte les étiquettes du projet (accepté par la politique réseau du service) et sa propre politique `betorun-addon-backup` (DNS, pods du projet, API).
- **Rangement** : `addon-backups/<service>/<sauvegarde>.gz` dans le stockage d'objets, table `addon_backups` (migration 000051). Les 7 dernières réussies sont gardées (`BackupsKept`), les échecs une semaine. Supprimer le service supprime ses copies.
- **Restauration** : nom retapé, double authentification. Une copie de l'état actuel est prise d'abord (déclencheur `before_restore`) ; sans elle, rien n'est rechargé. Le Job lit la sauvegarde par `GET /addons/backups/:taskID/download` (URL signée) et la recharge : la base `app` est recréée. Une seule copie ou restauration à la fois par service.
- **Prérequis** : cluster, stockage d'objets, `builds.secret`, `builds.public_api_url` ; sinon `503`.

## Agrandir le disque

`GET|POST /projects/:id/addons/:addonID/disk` (`addons.instances.get` / `addons.instances.update`) : même opération par copie que pour un conteneur (module [diskresize](diskresize.md)), tailles limitées à celles du modèle (`disks`), mode persistant seulement. Le service est arrêté pendant la copie ; `project_addons.disk` est mis à jour à la fin.

## Garde-fous

- **Porteur protégé** : `workloads.managed_by` = `addon/<modèle>/<id>` (migration 000045). Modifier, supprimer, dupliquer, revenir en arrière, toucher aux variables ou aux domaines du porteur par les routes des conteneurs → `409 MANAGED_BY_SERVICE`. La console l'indique et masque ces actions.
- **Démarrage** : un conteneur branché reçoit un init `wait-network` (busybox, non root) qui attend que chaque service branché réponde sur son port (60 s au plus par cible). Il évite les refus de connexion des premières secondes d'un pod neuf, avant que k3s n'ouvre le réseau du projet. Vaut aussi pour les bases PostgreSQL branchées.
- **Réseau** : comme tout conteneur, joignable seulement depuis le projet (politique réseau du chart).

## CLI

`betorun addons` (alias `services`) ; `betorun help addons` pour le détail.

```bash
betorun addons catalog                                   # versions, usages, tailles, « Bientôt »
betorun addons create redis files-taches --attach api --attach worker
betorun addons create redis cache --mode cache --size mini
betorun addons create mariadb boutique-db --attach api    # DATABASE_URL=mysql://…
betorun addons list                                      # état, taille, adresse, conteneurs branchés
betorun addons attach cache web [--variable QUEUE_URL]
betorun addons upgrade files-taches [--version 9.1] [-y] # confirmation si la version majeure change
betorun addons delete files-taches [-y]                  # nom retapé, double authentification
betorun addons notify mariadb [--off]                    # « Me prévenir »
```

`create` vérifie les conteneurs de `--attach` avant de créer, branche, puis attend que le service soit en service. `list`, `catalog` et `create` acceptent `--json`.

## Assistants (MCP de la CLI, Copilot)

- MCP (`betorun mcp`) : `list_addons`, `create_addon` (`template` redis, mariadb ou meilisearch ; `attach_to` : donne la variable de connexion au conteneur), `attach_addon`.
- Copilot de la console : `list_addons`, `create_addon`, `attach_addon` (dérivés des routes).
- Suppression et mise à jour restent à la personne.
