# Module `databases`

> Bases Postgres gérées d'un projet (CloudNativePG) : création, état, branchement sur un conteneur, restauration depuis les sauvegardes, import d'un dump.

## Modèle

Une base = un Cluster CloudNativePG `db-<nom>` dans l'espace du projet, une instance, un disque (1, 2, 5, 10 ou 20 Gi). CloudNativePG crée l'utilisateur et la base `app` et le secret `db-<nom>-app` (URI de connexion). L'API ne renvoie jamais le mot de passe : il n'est remis qu'aux conteneurs, par l'attachement (variable secrète, `DATABASE_URL` par défaut). Une politique réseau n'ouvre le port 5432 qu'aux pods du projet et à l'opérateur. Sauvegardes nocturnes si `databases.backup.*` est configuré.

## Import d'un dump

`POST /projects/:id/databases/:databaseID/import` (multipart : `dump`, `clean`), permission `databases.databases.import` (sensible, éditeur). CLI : `betorun databases import <base> <fichier.dump> [--clean]`.

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

## Fichiers clés

- [service.go](../../internal/modules/databases/service.go) — création, état, attachement, restauration.
- [cnpg.go](../../internal/modules/databases/cnpg.go) — Cluster, sauvegardes, politique réseau.
- [import.go](../../internal/modules/databases/import.go), [import_job.go](../../internal/modules/databases/import_job.go) — import d'un dump.
