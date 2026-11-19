# Module `addons`

> Services additionnels d'un projet (ADR [0013](../adr/0013-services-additionnels.md)) : logiciels du catalogue (aujourd'hui Redis via Valkey) installés, surveillés et mis à jour par betorun, joignables seulement depuis les conteneurs du projet. Les bases PostgreSQL restent dans le module [databases](databases.md) ; la console montre les deux sur la page « Services ».

## Modèle

Un service = un **conteneur porteur** ordinaire de sorte `service` (Service Kubernetes TCP sur le vrai port, une instance, stratégie `Recreate`, disque en mode persistant) + une ligne `project_addons` (migration 000044). Mêmes quotas et même facture qu'un conteneur.

- **Catalogue** fermé, dans le code ([catalog.go](../../internal/modules/addons/domain/catalog.go)) : `redis` → images `valkey/valkey` figées (9.1.4 recommandée, 9.0.8, 8.1.12). Le titre est « Redis (via Valkey) » et le logo est celui de Valkey, jamais celui de Redis (marque de Redis Ltd).
- **Usages** : `persistent` (défaut ; `noeviction`, AOF sur disque ; files de tâches, sessions) ou `cache` (`allkeys-lru`, sans disque).
- **Tailles** : `mini` (256 Mi), `small` (512 Mi, défaut), `medium` (1 Gi). `maxmemory` ≈ 80 % de la limite, la mémoire réservée vaut la limite.
- **Disques** : 1, 2 ou 5 Gi en mode persistant, taille fixe (le stockage `local-path` ne sait pas agrandir ; voir l'ADR).
- **Mot de passe** : généré par l'API, posé comme variable SECRÈTE `ADDON_PASSWORD` du porteur (la commande y fait référence par `$(ADDON_PASSWORD)`, jamais en clair dans les arguments). Il n'est jamais renvoyé : seuls les conteneurs branchés le reçoivent, dans l'adresse de connexion.
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

« Me prévenir » (personnel, hors projet, tout utilisateur connecté) : `GET /addons/waitlist`, `PUT|DELETE /addons/waitlist/:template` (services annoncés seulement : MariaDB, Meilisearch). Table `addon_waitlist` (migration 000046), langue de la demande gardée. Quand un service passe dans le catalogue, le worker envoie un seul e-mail par demande (au démarrage puis toutes les heures ; un échec d'envoi est retenté).

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
betorun addons list                                      # état, taille, adresse, conteneurs branchés
betorun addons attach cache web [--variable QUEUE_URL]
betorun addons upgrade files-taches [--version 9.1] [-y] # confirmation si la version majeure change
betorun addons delete files-taches [-y]                  # nom retapé, double authentification
betorun addons notify mariadb [--off]                    # « Me prévenir »
```

`create` vérifie les conteneurs de `--attach` avant de créer, branche, puis attend que le service soit en service. `list`, `catalog` et `create` acceptent `--json`.

## Assistants (MCP de la CLI, Copilot)

- MCP (`betorun mcp`) : `list_addons`, `create_addon` (`attach_to` : donne `REDIS_URL` au conteneur), `attach_addon`.
- Copilot de la console : `list_addons`, `create_addon`, `attach_addon` (dérivés des routes).
- Suppression et mise à jour restent à la personne.
