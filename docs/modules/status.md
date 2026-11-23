# Module status

Page de statut publique : état de chaque service, incidents en cours et
récents, maintenances programmées. Les états sont calculés par des **sondes**
qui ne lisent que la base (aucun appel externe depuis l'API) ; l'équipe
annonce les incidents et les maintenances depuis le back-office.

Code : `internal/modules/status` (`domain`, `service`, `repository`),
câblage `internal/infrastructure/router/status_wiring.go`, migration
`000067_status` (tables `status_incidents`, `status_incident_updates`).

## Composants et sondes

Registre : `service.DefaultComponents` (ordre d'affichage). Noms traduits par
le catalogue `core/i18n/catalog/status.json`.

| Clé | Nom | Sonde |
|---|---|---|
| `api` | Console et API | `operational` dès que la requête est servie |
| `vps` | Serveurs VPS : commande et gestion | voir ci-dessous ; `unknown` si les VPS sont désactivés |
| `domains` | Noms de domaine | commande payée (`domain_orders.status = 'ordering'`) restée plus d'une heure : `degraded` ; `unknown` si la vente de domaines n'est pas ouverte |

Sonde `vps` (SQL direct sur les tables du module compute) :

- dernière passe de réconciliation terminée (`compute_reconcile_runs`) : en
  erreur → `degraded` ; il y a plus de 30 min → `degraded` ; aucune passe
  jamais lancée → `unknown` ; première passe lancée il y a plus de 30 min et
  jamais terminée → `degraded` ;
- `compute_requests` : une demande `pending` depuis plus de 15 min (relais ou
  mode compute arrêté) ou `started` depuis plus de 6 h → `degraded`.

Une erreur de sonde donne `unknown`. Pour ajouter un composant : sa clé et son
nom (`domain`), l'entrée du catalogue, sa sonde (`service.Probe`) dans le
registre.

États : `operational` < `degraded` < `outage` ; `unknown` n'aggrave rien.
Un incident **actif** impose son impact aux composants touchés s'il est pire :
`minor` → `degraded`, `major` → `outage`, `maintenance` → `degraded`, et
seulement pendant sa fenêtre `starts_at`–`ends_at`. État d'ensemble : le pire
des composants (`unknown` si aucun n'est connu).

## API

### Public : `GET /kmanager/v1/status?lang=fr|en`

Sans authentification, `Cache-Control: public, max-age=30` ; le calcul est
gardé 30 s par process (vidé à chaque modification d'incident), un seul
calcul à la fois, détaché de la requête (5 s au plus). Calcul en échec : la
dernière page (moins de 5 min) est servie et le calcul n'est retenté qu'après
5 s. Langue :
`lang`, sinon celle de la requête. Rien d'interne : ni raison de sonde, ni
compte, ni auteur.

```json
{
  "overall": "degraded",
  "components": [ { "key": "vps", "name": "Serveurs VPS : commande et gestion", "state": "degraded" } ],
  "incidents": [ { "id": "…", "title": "…", "message": "…", "impact": "minor",
      "status": "monitoring", "components": ["vps"], "started_at": "…", "resolved_at": null,
      "starts_at": null, "ends_at": null,
      "updates": [ { "status": "monitoring", "message": "…", "created_at": "…" } ] } ],
  "maintenances": [ { "…": "même forme, à venir, la plus proche d'abord" } ],
  "updated_at": "…"
}
```

`incidents` : non résolus (maintenances en cours comprises) et résolus depuis
7 jours, les plus récents d'abord ; `updates` du plus récent au plus ancien.
`maintenances` : maintenances programmées pas encore commencées.

### Équipe (`/admin/status`, interne, `platform.status.manage`)

| Méthode | Chemin | Rôle |
|---|---|---|
| GET | `/admin/status` | états recalculés sans cache : `{overall, components: [{key, name, state, probe_state, reason}], computed_at}` |
| GET | `/admin/status/incidents?open=&limit=` | incidents (avec leurs messages), les plus récents d'abord ; `open=true` : non résolus ; `limit` 1–200, 50 par défaut |
| POST | `/admin/status/incidents` | annoncer un incident ou programmer une maintenance |
| GET | `/admin/status/incidents/:incidentID` | un incident et son fil |
| PUT | `/admin/status/incidents/:incidentID` | corriger textes, composants, impact, fenêtre, `started_at` (le statut ne change pas) |
| POST | `/admin/status/incidents/:incidentID/updates` | `{status, message_fr, message_en}` : nouveau message, nouveau statut (`resolved` clôt) |
| POST | `/admin/status/incidents/:incidentID/resolve` | `{message_fr?, message_en?}` : clôt (message par défaut « Incident résolu. ») |

Corps de création / modification : `title_fr, title_en, message_fr,
message_en` (obligatoires), `components` (clés connues, au moins une),
`impact` (`minor|major|maintenance`), `status` (création seulement ; par
défaut `investigating`, `identified` pour une maintenance), `started_at`
(défaut : maintenant, ou `starts_at`), `starts_at` / `ends_at` (obligatoires
pour une maintenance, fin après début). Le message de création devient le
premier message du fil.

Incident renvoyé : `id, title_fr, title_en, message_fr, message_en,
components, impact, status, started_at, resolved_at, starts_at, ends_at,
created_by, created_at, updated_by, updated_at, updates: [{id, status,
message_fr, message_en, created_by, created_at}]` (fil du plus ancien au plus
récent).

`reason` des sondes (équipe seulement) : `reconcile_failed`,
`reconcile_stale`, `reconcile_never_ran`, `requests_pending_stuck`,
`requests_started_stuck`, `domain_orders_stuck`, `probe_failed`, `disabled`.

Erreurs : 404 `NOT_FOUND` (incident), 409 `INCIDENT_RESOLVED` (message sur un
incident clos), 400 `INVALID_INPUT` (textes manquants, titre > 200 ou message
> 5 000 caractères, impact, statut, composants, fenêtre de maintenance).
