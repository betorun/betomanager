# Module support (tickets)

Demandes des clients à l'équipe betorun, traitées au back-office. Même
fonctionnement et mêmes réponses JSON que le support de SYK
(`scapi/internal/modules/support`), adapté à betorun : un ticket appartient
à une **organisation** (pas d'étage « école »), peut viser un projet et une
ressource (`resource_kind` = `vps`…, `resource_id`). Marque blanche : aucun
fournisseur n'est nommé, ni dans les textes ni dans les e-mails.

Code : `internal/modules/support` (contrôleurs), `domain/` (types, règles,
transitions), `service/` (cas d'usage, ports, e-mails), `repository/`
(Postgres) ; câblage `internal/infrastructure/router/support_wiring.go`.
Migration `000065_support` : `support_tickets`, `support_messages`,
`support_attachments`, `support_ticket_events`, `support_macros`.

## Modèle

- **Ticket** : `subject` (3–150 caractères), `description` (la demande
  initiale, hors `messages`, 10 000 caractères au plus), `status`,
  `priority`, `category`, `tags` (10 au plus, minuscules), `assigned_to_user_id`,
  `created_by_user_id` / `created_by_email`, `last_activity_at`, `closed_at`.
- **Catégories** : `BILLING`, `TECHNICAL`, `ABUSE`, `REQUEST` (ouverture du
  port 25, déblocage d'une adresse…), `BUG`, `FEATURE_REQUEST` ; facultative.
- **Priorités** : `LOW`, `MEDIUM` (défaut), `HIGH`, `URGENT` ; le client la
  propose à la création, l'équipe la change.
- **Messages** : `author_role` = `CUSTOMER` | `SUPPORT_AGENT` | `SYSTEM`,
  `author_user_id`, `body` (texte brut). Pas de note interne.
- **Événements** (journal) : `TICKET_CREATED`, `STATUS_CHANGED`, `ASSIGNED`,
  `PRIORITY_CHANGED`, `TAG_ADDED`, `TAG_REMOVED`, `COMMENT_ADDED` ; `payload`
  (`status`/`from`, `priority`/`from`, `tag`, `assigned_to`, `by`…).
- **Pièces jointes** : PDF, JPEG, PNG, WebP, texte (type reconnu au contenu,
  pas à l'extension), 10 Mo, 20 par ticket. Envoyées **à l'API** (multipart)
  et rangées dans le stockage objet (`support/<org>/<ticket>/<id>`) ;
  téléchargées par l'API (`Content-Disposition: attachment`, `nosniff`).
  Sans stockage objet : `503 ATTACHMENTS_UNAVAILABLE`. La limite de 20 est
  comptée sous verrou du ticket (deux envois simultanés ne la dépassent pas) ;
  un fichier déposé mais non enregistré est effacé du stockage.
- **Réponses types** (macros) : communes à l'équipe (`title`, `body`, `category`).

## Statuts

```
OPEN ──réponse équipe──▶ WAITING_CUSTOMER ──réponse client──▶ IN_PROGRESS
  ▲                                                               │
  └──── rouvrir (client) ◀── RESOLVED ◀── résoudre (client/équipe)┘
                              CLOSED (équipe) : définitif pour le client
```

- Réponse de l'équipe : `WAITING_CUSTOMER`. Réponse du client : depuis
  `WAITING_CUSTOMER` → `IN_PROGRESS`, depuis `RESOLVED` → `OPEN`, sur `CLOSED` :
  refus `409 TICKET_CLOSED`.
- Le client résout (`RESOLVED`, `closed_at` posé) et rouvre un ticket résolu
  (`OPEN`) ; un ticket fermé ne se rouvre pas (`409 TICKET_CLOSED`).
- L'équipe fixe librement le statut (`PATCH`), y compris rouvrir un ticket fermé.

## API

Client (`/organizations/:id/support`, droits vérifiés sur l'organisation) :

| Méthode | Chemin | Permission |
|---|---|---|
| POST | `/tickets` | `support.tickets.create` |
| GET | `/tickets?status&category&priority&search&limit&offset` | `support.tickets.list` |
| GET | `/tickets/:ticketID` | `support.tickets.get` |
| POST | `/tickets/:ticketID/messages` | `support.tickets.create` |
| POST | `/tickets/:ticketID/attachments` (multipart `file`, `message_id`) | `support.tickets.create` |
| GET | `/tickets/:ticketID/attachments/:attachmentID` | `support.tickets.get` |
| PATCH | `/tickets/:ticketID/resolve` | `support.tickets.create` |
| PATCH | `/tickets/:ticketID/reopen` | `support.tickets.create` |

Équipe (`/admin/support`, interne, `platform.support.manage`) : `GET /summary`
(`open`, `in_progress`, `waiting_customer`, `high_priority` pour le badge du
menu), `GET /tickets` (mêmes filtres + `organization_id` ; à traiter d'abord,
puis priorité, plus ancienne activité d'abord), `GET|PATCH /tickets/:ticketID`,
`POST /tickets/:ticketID/assign`, `POST /tickets/:ticketID/messages`,
`GET /tickets/:ticketID/attachments/:attachmentID`, `GET|POST /macros`,
`PATCH|DELETE /macros/:macroID`.

Back-office (migration 000076) :

- `GET /summary` compte aussi `mine` (tickets ouverts de l'agent) et
  `unassigned`.
- `GET /tickets?assigned=me|none|<agent>` ; chaque ticket de la file porte
  `organization_name`, `assigned_to {user_id, email, full_name}`,
  `last_customer_message_at` (création à défaut) et `first_response_at`
  (première réponse de l'équipe, notes exclues).
- `GET /agents` : comptes ayant un rôle sur `platform` (directement ou par un
  groupe). L'assignation n'accepte qu'un de ces comptes.
- `GET /tickets/:ticketID/context` : organisation et propriétaire,
  porte-monnaie (solde, devise, état), projets avec leur nombre de
  conteneurs, sites, VPS et bases, ressource rattachée, dix autres tickets de
  l'organisation.
- `POST /tickets/:ticketID/notes` `{body}` : note interne (`internal: true`,
  événement `NOTE_ADDED`). Ni e-mail, ni statut changé, ni publication en
  direct ; jamais dans la vue du client, et aucune pièce jointe du client ne
  peut s'y rattacher.
- `POST /tickets/bulk` `{ids, status?, priority?, assign_to?}` (50 tickets au
  plus, `assign_to` : `me`, `none` ou un agent) : chaque ticket est traité
  seul, `{results: [{id, ok, error?}]}` dans l'ordre demandé. Un motif commun
  (`X-Betorun-Justification`), inscrit au journal de la plateforme ; chaque
  changement entre dans le journal de son ticket.

Les routes d'un ticket précis exigent le motif et sont visibles dans le
journal d'audit du client.

Réponses : `{ticket, messages, attachments, events}` pour un ticket (côté
équipe, le ticket est enrichi comme dans la file et les notes sont incluses),
`{items, total}` pour une liste (`limit` 20 par défaut, 100 au plus),
`{macro}` / `{items}` pour les réponses types.

Rôles : lecteur (`roles/viewer`) liste et lit ; développeur et au-dessus
ouvrent et répondent. `platform.support.manage` est dans
`roles/platform.admin` et `roles/platform.support`.

## E-mails

Par `mail.Sender`, en arrière-plan, dans la langue de la personne ; un échec
est journalisé et ne fait jamais échouer la requête. Accusé de réception au
créateur ; réponse de l'équipe au créateur et aux personnes de l'organisation
qui ont écrit dans le ticket ; ticket résolu ou fermé par l'équipe : au
créateur. Rien à l'équipe : le compteur `/admin/support/summary` suffit.
Lien vers la console : `<frontend.base_url>/support/<ticket>`.

## Tests

`domain` et `service` : tests unitaires. Parcours complet sur Postgres :

```
bash scripts/iam_e2e_db.sh
IAM_E2E=1 go test ./internal/modules/support/... -count=1
docker rm -f iam-e2e-pg
```
