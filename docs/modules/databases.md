# Module `databases`

> Bases Postgres gérées d'un projet (CloudNativePG) : création, état, branchement sur un conteneur, restauration depuis les sauvegardes, import d'un dump, contenu, remise à zéro.

## Modèle

Une base = un Cluster CloudNativePG `db-<nom>` dans l'espace du projet, une instance, un disque (1, 2, 5, 10 ou 20 Gi). CloudNativePG crée l'utilisateur et la base `app` et le secret `db-<nom>-app` (URI de connexion). L'API ne renvoie jamais le mot de passe : il n'est remis qu'aux conteneurs, par l'attachement (variable secrète, `DATABASE_URL` par défaut). Une politique réseau n'ouvre le port 5432 qu'aux pods du projet et à l'opérateur. Sauvegardes nocturnes si `databases.backup.*` est configuré.

## Import d'un dump

`POST /projects/:id/databases/:databaseID/import` (multipart : `dump`, `clean`, `reset`), permission `databases.databases.import` (sensible, éditeur). CLI : `betorun databases import <base> <fichier.dump> [--clean]`.

- **Format** : seul le format personnalisé de pg_dump (`pg_dump -Fc`, en-tête `PGDMP`) est accepté. Un dump SQL brut passerait par psql, dont les méta-commandes (`\!`) lancent des commandes dans le Job. 2 Gio au plus (`databases.import_max_bytes`).
- **Déroulé** : le dump va dans le stockage d'objets (`database-imports/<id>.pgc`), puis un Job `db-import-<id>` est créé dans l'espace du projet ([import_job.go](../../internal/modules/databases/import_job.go)) :
  - un conteneur d'amorce (`databases.import_fetch_image`, défaut `curlimages/curl` figé) télécharge le dump par une URL signée de l'API (`/databases/imports/:id/dump`, 2 h, même secret que les builds, signature propre aux dumps) ;
  - `pg_restore --no-owner --no-acl --exit-on-error -d app` (et `--clean --if-exists` si demandé), image Postgres de la base (même version), utilisateur `app` lu dans son secret, jamais un superutilisateur ;
  - une tentative, une heure au plus, nettoyé après 24 h ; sans jeton Kubernetes ni privilège.
- **Réseau** : la politique `betorun-db-import` ne laisse au Job que le DNS, les bases du projet (5432), l'Internet public et les adresses privées de l'API (`builds.egress_allow_cidrs`).
- **État** : relevé à la lecture (`GET …/imports`, `GET …/imports/:importID`) : `RUNNING`, puis `SUCCEEDED` ou `FAILED` avec la fin du journal de pg_restore. Terminé, le dump est effacé. Un seul import à la fois par base. Prérequis : stockage d'objets, `builds.secret` et `builds.public_api_url` ; sinon `503`.
- **Sans --clean**, un objet déjà présent fait échouer l'import (rien n'est modifié au-delà de ce qui précède l'erreur : pg_restore n'utilise pas de transaction unique).

## Branchements et dernier import

Migration 000031 `database_attachments` (base, conteneur, variable) : chaque `POST /attach` réussi est tracé (au mieux : une trace manquante ne fait pas échouer le branchement). La vue d'une base porte `attachments` (conteneurs encore présents) et `last_import`. La console les montre sur la carte, et l'avertissement « Remplacer les objets existants » nomme les conteneurs branchés. La place réservée du projet (vue d'ensemble) compte les disques des bases.

## Contenu, inspection d'un dump, remise à zéro

Lecture et écriture SQL sans exec (la plateforme n'a pas `pods/exec`, voir [rbac.yaml](../../deploy/k8s/rbac.yaml)) : un **Job** `db-<tâche>-<id>` dans l'espace du projet ([task_job.go](../../internal/modules/databases/task_job.go)), connecté avec l'utilisateur `app` (secret `db-<nom>-app`, jamais un superutilisateur), soumis à la politique réseau des imports. Le conteneur attend la base (`pg_isready`, les premières connexions d'un pod neuf sont refusées tant que la politique réseau ne l'a pas pris en compte). L'API attend la fin du Job, lit son journal, puis le supprime. Compter 5 à 10 s par mesure.

| Méthode | Chemin | Permission | Description |
|---------|--------|-----------|-------------|
| `GET` | `/:databaseID/contents` | `databases.databases.get` | Taille et tables par schéma (`{size_bytes, tables, schemas[], measured_at}`) ; 409 si la base n'est pas prête |
| `POST` | `/:databaseID/import/inspect` | `databases.databases.get` | Multipart `dump` : le **début** du fichier (16 Mio suffisent). Table des matières lue en Go ([dumptoc.go](../../internal/modules/databases/domain/dumptoc.go), formats d'archive 1.10 à 1.16, PG 9.0 à 17) et comparée à la base : `conflicts` (schémas ayant des tables des deux côtés), `foreign` (base non vide sans schéma commun : sans doute la mauvaise base), `complete` false si le fichier est tronqué |
| `POST` | `/:databaseID/reset` | `databases.databases.reset` (éditeur, sensible) | `{confirm_name}` = nom de la base. Copie de sécurité puis remise à zéro → `{snapshot: {taken_at, expires_at, size_bytes}}` |
| `POST` | `/:databaseID/reset/undo` | `databases.databases.reset` (sensible) | Réimporte la copie (import ordinaire avec `--clean`) → 202 `DatabaseImport` ; 404 sans copie ou copie expirée |

- **Remise à zéro** : un seul Job. Amorce `dump` (`pg_dump -Fc --no-owner --no-acl`), amorce `upload` (curl, `PUT /databases/snapshots/:taskID/upload`, URL signée 30 min, acceptée une seule fois et seulement pendant la tâche), puis `main` : coupe les autres connexions de `app`, puis `DROP OWNED BY CURRENT_USER CASCADE` en transaction. Si la copie n'arrive pas, rien n'est vidé. La base, l'utilisateur, son mot de passe et les branchements restent.
- **Copie** : `database-snapshots/<base>/<tâche>.pgc` dans le stockage d'objets, une par base (migration 000032 `database_snapshots`), gardée 24 h (effacée au passage après expiration, et avec la base). Vider une base déjà vide garde la copie précédente : un double clic ne remplace pas la bonne copie par du vide. Une remise à zéro à la fois par base (verrou en mémoire).
- **Import avec « vider d'abord »** : champ multipart `reset=true` ; exige aussi `databases.databases.reset`.
- **Prérequis** : ceux de l'import (stockage d'objets, `builds.secret`, `builds.public_api_url`) ; sans eux, `503` (pas de remise à zéro sans copie possible).
- **Console** : carte de la base (taille, tables par schéma, Actualiser), « Vider la base » (nom à retaper, conteneurs branchés arrêtés puis redémarrables), bandeau de la copie et « Restaurer la copie », aperçu du dump dans le dialogue d'import.

## Fichiers clés

- [service.go](../../internal/modules/databases/service.go) — création, état, attachement, restauration.
- [cnpg.go](../../internal/modules/databases/cnpg.go) — Cluster, sauvegardes, politique réseau.
- [import.go](../../internal/modules/databases/import.go), [import_job.go](../../internal/modules/databases/import_job.go) — import d'un dump.
- [contents.go](../../internal/modules/databases/contents.go), [task_job.go](../../internal/modules/databases/task_job.go) — contenu, inspection, remise à zéro et copie de sécurité.
- [domain/dumptoc.go](../../internal/modules/databases/domain/dumptoc.go) — lecture de la table des matières d'un dump `-Fc`.
