# ADR 0018 — Catalogue des régions tenu dans le back-office

Statut : accepté (2026-11-26).

## Contexte

Une région n'était qu'un texte libre saisi sur la fiche d'un cluster (`clusters.region`). La plateforme connaissait les régions en relisant les clusters. La console tenait en dur, dans son code et ses traductions, le libellé de cinq codes (`eu-west-gra`, `eu-west-rbx`, `eu-west-sbg`, `af-west-dkr`, `af-central-bzv`). Pour toute autre région, elle affichait le code brut.

Trois problèmes en découlaient :

- **Ajouter une région demandait une modification de la console**, avec deux traductions, puis un déploiement. Or betorun doit ouvrir de nombreuses régions en Afrique.
- **Une faute de frappe sur un cluster créait une région** : rien ne contrôlait le texte au-delà de son format.
- **Le back-office ne pouvait rien préparer avant le premier cluster.** La page « Pays et régions » n'acceptait qu'une région déjà portée par un cluster : sans cluster, le bouton « Ajouter » restait grisé, sans explication.

## Décision

### 1. Les régions forment un catalogue

`region_settings`, qui ne portait que l'adresse de mesure de latence, devient le **catalogue des régions**. Chaque région a :

- un **code** définitif (`af-central-bzv`) : clusters, projets et pays s'y réfèrent, il ne change jamais ;
- une **zone** prise dans une liste fermée : `af-west`, `af-central`, `af-east`, `af-north`, `af-south`, `eu-west`, `eu-central`, `eu-south` ;
- un **pays** (ISO 3166-1 alpha-2) et une **ville** ;
- son **adresse de mesure** (inchangée).

L'équipe ajoute, modifie et retire les régions depuis le back-office (`POST`, `PUT`, `DELETE /admin/regions`). Une région portée par un cluster ou suggérée pour un pays ne se retire pas (`409 REGION_IN_USE`).

### 2. Le libellé se construit, il ne se traduit pas région par région

La console compose « Afrique centrale · Brazzaville (Congo) » à partir de trois éléments :

- la **zone**, traduite une fois pour toutes dans la console ;
- le **pays**, nommé par le navigateur (`Intl.DisplayNames`) dans la langue de l'utilisateur ;
- la **ville**, nom propre gardé tel quel.

Ajouter la trentième région ne demande donc ni traduction ni déploiement. Seule une nouvelle **zone** demande un libellé dans la console, ce qui reste rare.

`GET /regions` (clients) rend `zone`, `country` et `city` avec chaque région, et `GET /admin/regions` les rend pour le back-office.

### 3. Un cluster ne prend qu'une région du catalogue

À la déclaration d'un cluster, et quand sa région change, l'API exige une région vide ou présente au catalogue (`400 UNKNOWN_REGION`). Dans la console, le champ devient un menu déroulant.

Un cluster resté sur une région hors catalogue garde ses autres modifications possibles, pour ne bloquer personne. La migration rend ce cas théorique.

### 4. Pays → région : toute région du catalogue

La table pays → région accepte une région du catalogue même sans cluster. L'équipe peut ainsi préparer une ouverture. La région n'est suggérée aux clients qu'une fois proposée, c'est-à-dire avec au moins un cluster `active`. Cette règle était déjà celle de `GET /regions`.

### 5. Reprise de l'existant (migration 000082)

- les cinq régions que la console connaissait reçoivent leur zone, leur pays et leur ville ;
- les régions portées par un cluster ou associées à un pays entrent au catalogue **à compléter** : zone, pays et ville vides. La console les signale et propose de les compléter ;
- la base contrôle le code, la zone et le pays, et impose que zone, pays et ville soient remplis tous les trois ou aucun.

## Conséquences

- La liste des cinq régions écrite en dur dans la console disparaît, avec leurs traductions.
- La page « Pays et régions » du back-office devient l'endroit où l'on crée une région ; la fiche cluster la choisit dans une liste.
- `PUT /admin/regions/:code` accepte toujours `{probe_url}` seul : un appel existant garde son effet.
