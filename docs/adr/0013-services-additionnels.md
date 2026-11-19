# ADR 0013 — Services (Valkey compatible Redis d'abord, PostgreSQL inclus)

Statut : accepté. Étapes 1 et 2 réalisées le 2026-11-19 (sorte `service`, module `addons`, API) ; page Services et CLI faites (voir « État de réalisation »).

## Problème

Une vraie application a besoin d'autre chose que PostgreSQL : un cache et des
files de tâches (Redis : sessions, Sidekiq, BullMQ, Celery, files Laravel), souvent
MySQL/MariaDB (PHP, WordPress), parfois une recherche (Meilisearch). Aujourd'hui
betorun ne propose que PostgreSQL géré.

Un client peut déjà lancer l'image `valkey/valkey` comme conteneur, mais :

- en `worker`, aucun Service n'est créé : les autres conteneurs ne le joignent
  pas par son nom ;
- en `web`, le Service est pensé pour HTTP (port 80 → port cible, sondes HTTP,
  Ingress) : pas adapté à un protocole TCP comme Redis (6379) ;
- aucun mot de passe n'est généré, et rien ne pose `REDIS_URL` dans les
  conteneurs qui l'utilisent.

## Ce qui existe déjà et qu'on réutilise

- **Chart `standard-app`** embarqué ([charts.go](../../internal/infrastructure/helm/charts/charts.go)),
  installé par Temporal dans l'espace du projet : sortes `web` / `worker` / `cron`,
  disque persistant (`persistence_enabled`, facturé au compteur `disk`),
  `command` / `args`, sonde par commande (`health_command`, worker), pods non root,
  `seccomp`, pas de jeton Kubernetes.
- **Réseau privé du projet** ([network-policy.yaml](../../internal/infrastructure/helm/charts/standard-app/templates/network-policy.yaml)) :
  les conteneurs d'un même projet se joignent sur tous les ports ; jamais un
  autre projet.
- **Branchement des bases** (`databases.Service.Attach`) : l'adresse de connexion
  est posée comme variable secrète du conteneur (`Workloads.SetSecret`, puis
  redéploiement), tracée dans `database_attachments`, jamais renvoyée par l'API.
- **Quotas et facturation** : un conteneur et son disque sont déjà comptés
  (limites du projet, compteurs `compute` et `disk`). Rien à ajouter côté prix.

## Options écartées

- **Place de marché de charts Helm tiers.** Un chart peut créer des objets
  globaux (ClusterRole, webhooks, CRD) et sortir de l'isolation des projets ; il
  faudrait relire chaque chart et traduire n'importe quelle ressource en
  facturation. Les charts Bitnami, source habituelle, ne sont plus maintenus
  gratuitement depuis 2025.
- **Un opérateur par logiciel** (comme CloudNativePG pour PostgreSQL). Réservé aux
  services qu'on voudra vraiment gérer (sauvegardes, haute disponibilité), un par
  un, plus tard.

## Décision proposée

Un **catalogue fermé de modèles**, posés sur le chart actuel. Un service
additionnel est un conteneur ordinaire (même cycle de vie, mêmes quotas, même
facture), créé à partir d'un modèle maintenu par betorun.

### 1. Nouvelle sorte de conteneur : `service` (TCP interne)

Dans `workloads/domain/kind.go` et le chart :

| | `web` | `service` (nouveau) |
|---|---|---|
| Service Kubernetes | port 80 → port cible, nommé `http` | **le vrai port** (ex. 6379), nommé `tcp` |
| Adresse publique, Ingress, domaines, TLS | oui | **jamais** |
| Sondes | HTTP | **TCP** (`tcpSocket`) ou commande |
| Réseau | Ingress controller + projet | **projet seulement** |
| Autoscaling, PDB | possibles | non (une instance, état sur disque) |

Le client peut aussi créer un conteneur `service` à la main (image de son choix) :
utile au-delà du catalogue (un serveur gRPC interne, par exemple).

Point à vérifier : le chart force `runAsUser: 1000` et `runAsNonRoot`. L'image
Valkey s'en accommode (données sous `/data`, groupe du disque). L'image officielle
MariaDB démarre en root puis change d'utilisateur : son modèle devra la lancer
directement sous son utilisateur (`999`) ; à tester avant de l'ouvrir.

### 2. Module `addons` : modèles et instances

Modèles **dans le code** (pas en base) : ajouter un logiciel = une revue de code.

```go
type Template struct {
    ID        string   // "redis"
    Name      string   // « Redis (via Valkey) » : le mot que les clients cherchent
    Logo      string   // clé du logo SVG côté console ("valkey")
    Versions  []Version // versions maintenues, la plus récente d'abord (image figée par version)
    Port      int      // 6379
    Command   []string // ["valkey-server", "--requirepass", "$(ADDON_PASSWORD)", "--maxmemory", "$(ADDON_MAXMEMORY)", …]
    Modes     []Mode   // redis : "persistent" (défaut, disque, appendonly yes) ou "cache" (sans disque, allkeys-lru)
    DiskPath  string   // "/data"
    Sizes     []string // tailles proposées (grille existante)
    Disks     []string // "1Gi", "2Gi", "5Gi"
    URIFormat string   // "redis://default:{password}@{host}:{port}"
    Variable  string   // "REDIS_URL"
    Health    []string // ["valkey-cli", "-a", "$(ADDON_PASSWORD)", "ping"]
}

type Version struct {
    ID    string // "8.1"
    Image string // "valkey/valkey:8.1.4" — étiquette précise, correctifs posés par nous
    Major int    // 8 : un changement de majeure est signalé avant confirmation
}
```

Table `project_addons` (migration 000044) :

| Colonne | Rôle |
|---|---|
| `id` uuid | identifiant |
| `project_id` uuid, FK | une instance appartient à un projet |
| `workload_id` uuid, FK | le conteneur qui la porte (sorte `service`) |
| `template` text | `redis`, plus tard `mariadb`… |
| `version` text | version choisie (`8.1`) ; l'image précise vient du modèle |
| `mode` text | `persistent` ou `cache` (redis) |
| `created_by`, `created_at` | suivi |

Table `addon_attachments` (comme `database_attachments`) : instance, conteneur
branché, variable.

**Mot de passe** : généré par l'API (32 octets aléatoires), posé comme variable
**secrète** du conteneur porteur (`ADDON_PASSWORD`, chiffrée comme les autres
variables). Kubernetes remplace `$(ADDON_PASSWORD)` dans la commande. L'API ne le
renvoie jamais ; il n'est remis qu'aux conteneurs branchés, dans l'adresse.

**Brancher** : construit l'adresse (`redis://default:…@<nom>:6379`, nom = Service
du conteneur porteur) et la pose avec `Workloads.SetSecret`, exactement comme une
base. Le conteneur branché est redéployé.

**Supprimer** : supprime le conteneur porteur et son disque (saga existante), retire
la variable des conteneurs branchés ? *Non* : on la laisse et on prévient (un
conteneur qui perd sa variable sans prévenir planterait au redémarrage) ; la console
liste les conteneurs encore branchés avant de confirmer.

### 3. API

```
GET    /projects/:id/addons/templates              catalogue (nom, tailles, disques, prix indicatif)
GET    /projects/:id/addons                        instances + état du conteneur porteur
POST   /projects/:id/addons                        {template, name, version?, mode?, size, disk}
POST   /projects/:id/addons/:addonID/upgrade       {version?}  (défaut : dernière version maintenue)
DELETE /projects/:id/addons/:addonID
POST   /projects/:id/addons/:addonID/attach        {workload_id, variable?}  (REDIS_URL par défaut)
```

Permissions IAM nouvelles, calquées sur les bases : `addons.instances.list`,
`.get`, `.create`, `.update` (changer de version), `.delete`, `.connect` (brancher).
Rôles : lecteur voit ; développeur branche ; éditeur crée, met à jour et supprime.
`.delete` sensible (double authentification récente exigée).

### 4. Console et CLI

- Console : page **« Services »**, qui **remplace « Bases de données »** dans la
  navigation (`/databases` redirige vers `/services`). Une seule liste « vos
  services » (PostgreSQL et les autres, avec un repère du type) et un seul
  catalogue. Chaque carte affiche son niveau de prise en charge : PostgreSQL
  « Géré : sauvegardes et restauration », les modèles « Installé : sans
  sauvegarde ». Le backend reste séparé : PostgreSQL garde son module
  `databases` (CloudNativePG, sauvegardes, import, remise à zéro), la page
  appelle les deux API.
  Le catalogue est fait de **cartes illustrées**, pour donner envie (demande du 2026-11-19) : logo du
  logiciel en SVG (pas d'image distante), nom, une phrase d'usage (« Cache,
  sessions et files de tâches »), puces des usages types (Laravel, Sidekiq,
  BullMQ, Celery), prix « à partir de » tiré de la grille, bouton « Ajouter ».
  Survol avec légère élévation, entrée des cartes décalée de 30 ms, sombre et
  clair (règles de mouvement de la console). Les logos vivent avec ceux de la
  landing (`components/landing/logos.tsx`).
  **Marques** : le logo et le nom « Redis » appartiennent à Redis Ltd. Le service
  étant Valkey, la carte s'appelle **« Redis (via Valkey) »** (décision du
  2026-11-19 : les clients reconnaissent le mot Redis) et montre le **logo
  Valkey**, jamais le logo Redis : le nom dit ce que c'est vraiment. Même règle pour chaque modèle : logo du logiciel réellement installé,
  selon ses règles d'usage. Création (nom, taille, disque, prix estimé avec la grille), carte de
  l'instance (état, adresse interne sans mot de passe, conteneurs branchés,
  « Brancher sur un conteneur »). Le conteneur porteur reste visible dans
  « Conteneurs » avec un repère « géré par le service Redis » et ses actions
  sensibles (image, commande) bloquées.
- CLI : `betorun addons list | create redis <nom> | attach <nom> <conteneur> | delete`.
- Copilot : outils `list_addons` et `attach_addon` comme pour les bases.

### 5. Ce qui n'est PAS promis (V1)

Pas de sauvegarde, pas de haute disponibilité, pas de mise à jour automatique de
version. Le texte de la console le dit : « installé et surveillé par betorun ;
les données sont sur le disque du service, sans sauvegarde ». Pour un cache et des
files, c'est acceptable ; pour MariaDB, il faudra au moins une sauvegarde nocturne
(job `mariadb-dump` vers le stockage d'objets, comme l'import des bases) avant de
l'ouvrir.

## Ordre de réalisation

1. Sorte `service` (domaine, chart, tests de rendu du chart, validation).
2. Module `addons` (modèles, table, création, branchement, suppression) + IAM +
   journal d'audit.
3. Console : page « Services » (catalogue illustré, liste commune avec les bases
   PostgreSQL, redirection de `/databases`), CLI ; aides « (?) » de la page.
4. Modèle `redis` (Valkey) ouvert ; essai de charge simple (BullMQ / Sidekiq).
5. Ensuite seulement : `mariadb` (utilisateur non root + sauvegarde), puis
   `meilisearch`.

## Décisions (2026-11-19)

1. **Nom : « Services »**, une page pour tout, PostgreSQL compris (interface
   seulement ; backends séparés, voir Console). Carte **« Redis (via Valkey) »**
   avec le logo Valkey, jamais le logo Redis (marque de Redis Ltd).
   Logos : SVG officiels (Simple Icons, licence CC0 ; Valkey : dépôt
   valkey-io), aux couleurs de chaque marque.
2. **Versions** : le client choisit sa version à la création parmi celles qu'on
   maintient ; un bouton **« Mettre à jour »** propose la plus récente (correctifs
   compris). Jamais de retour en arrière (format des données). Un changement de
   version majeure est signalé avant confirmation. Pas de mise à jour automatique.
3. **Plafond : 10 services par projet** (hors bases PostgreSQL, qui gardent leurs
   règles), réglable par le staff comme `max_projects`
   (`projects.max_addons`, défaut 10). Le quota de ressources s'applique en plus.
4. **Redis : persistance par défaut** (`persistent` : disque, `appendonly yes`) :
   les files de tâches (Sidekiq, BullMQ, Laravel) ne doivent pas perdre de
   travail à un redémarrage. Option **« Cache seulement »** (`cache` : sans
   disque, `maxmemory-policy allkeys-lru`), moins chère. Dans les deux modes,
   `maxmemory` ≈ 80 % de la mémoire du conteneur : Redis refuse proprement une
   écriture (ou évince en mode cache) au lieu d'être arrêté faute de mémoire.

## État de réalisation (2026-11-19)

- **Fait** : sorte `service` (chart : Service TCP sur le vrai port, sondes TCP ou
  commande, stratégie `Recreate`, une instance ; console : 4e type de conteneur) ;
  module `addons` (catalogue `redis` : Valkey 9.1.4 recommandée, 9.0.8, 8.1.12 ;
  migration 000044 `project_addons`, `addon_attachments`, `projects.max_addons`) ;
  API `/projects/:id/addons` ; IAM ; outils Copilot `list_addons`,
  `create_addon`, `attach_addon`.
- **Vérifié sur le cluster** : un client sans mot de passe reçoit `NOAUTH` ;
  avec `REDIS_URL` il écrit et lit ; le disque (AOF) est monté ; le mot de passe
  n'apparaît ni dans l'API ni dans la commande du pod (`$(ADDON_PASSWORD)`).
- **Course au démarrage, réglée** : pendant les premières secondes d'un nouveau
  pod, k3s n'a pas encore ouvert le réseau du projet (« Connection refused »).
  Un conteneur branché reçoit désormais un init `wait-network` (busybox, non
  root) qui attend que chaque service branché réponde sur son port (60 s au
  plus par cible, `waitFor` du chart) ; vaut aussi pour les bases PostgreSQL
  branchées. Vérifié : la première tentative de l'application réussit.
- **Plafond réglable par le staff** : `PATCH /admin/projects/:id/max-addons`
  (0 à 100, permission `PlatformProjectsSetLimits`, route interne).
- **Conteneur porteur protégé** : colonne `workloads.managed_by`
  (migration 000045, « addon/redis/<id> », porteurs existants rattrapés) ;
  modifier, supprimer, dupliquer, revenir en arrière, toucher aux variables ou
  aux domaines du porteur est refusé (409 `MANAGED_BY_SERVICE`). La console
  l'indique (« Géré par le service … » dans la liste, bandeau sur la page) et
  masque ces actions.
- **Page « Services » (étape 3)** : `/services` dans la console (l'ancienne
  `/databases` y redirige) : liste commune des bases PostgreSQL (« Géré ») et
  des services installés (« Installé »), détail dépliable par ligne ;
  catalogue en cartes avec logos (Valkey pour Redis, jamais le logo Redis),
  filtres par catégorie, MariaDB et Meilisearch annoncés « Bientôt » ;
  ajout (nom, version, usage, taille, disque, branchement immédiat sur des
  conteneurs, prix estimé), branchement, mise à jour (avertissement de
  version majeure), suppression (double authentification).
- **« Me prévenir »** : les services annoncés (`domain.Announced` : MariaDB,
  Meilisearch) sont renvoyés par le catalogue (`upcoming`) ; chaque
  utilisateur peut demander à être prévenu (`PUT/DELETE /addons/waitlist/:template`,
  table `addon_waitlist`, migration 000046, langue de la demande gardée). Quand
  un service passe dans `domain.Catalog`, le worker envoie UN e-mail par
  demande (passage au démarrage puis toutes les heures, `notified_at`, échec
  d'envoi retenté au passage suivant).
- **Disque à taille fixe (décision 2026-11-19)** : le stockage du cluster
  (`local-path`) ne sait pas agrandir un disque ; pas d'agrandissement en V1.
  Piste retenue pour plus tard : migration par copie (nouveau disque, arrêt de
  quelques secondes, copie, redémarrage), ou un stockage extensible.
- **Usage réel des disques (décision 2026-11-19)** : `local-path` ne plafonne
  pas non plus la taille demandée. Le module [diskusage](../modules/diskusage.md)
  mesure chaque disque toutes les heures : alerte à 80 % et 100 %, conteneur
  arrêté à 120 % (données gardées, délai de grâce de 6 h après relance). Les
  bases sont mesurées et alertées, jamais arrêtées automatiquement.
- **CLI** : `betorun addons` (alias `services`) : `catalog`, `list`,
  `create <service> <nom>` (`--mode`, `--size`, `--disk`, `--version`,
  `--attach` répétable, attente du démarrage), `attach`, `upgrade`
  (confirmation sur changement de version majeure), `delete` (nom retapé ;
  double authentification), `notify` (« Me prévenir », `--off`). Outils MCP de
  la CLI : `list_addons`, `create_addon` (`attach_to`), `attach_addon`.
  Types propres à la CLI : aucune dépendance au module serveur.
- **Reste** : agrandissement du disque (voir la décision ci-dessus), nouveaux
  modèles (MariaDB, Meilisearch).

## Questions ouvertes

- Faut-il exposer les services hors du projet (accès depuis le poste du client,
  comme un tunnel `betorun services connect`) ? Pas en V1.
