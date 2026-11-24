# Module assistance (accès de l'équipe chez un client)

Décision : [ADR 0017](../adr/0017-acces-assistance.md). Par défaut, l'équipe betorun n'a **aucun droit** sur les ressources d'un client. Un accès d'assistance lui en ouvre quelques-uns, pour une personne nommée, pour une durée bornée. Le client le voit et peut le retirer.

Code : `internal/modules/assistance` (service, contrôleur) ; câblage [assistance_wiring.go](../../internal/infrastructure/router/assistance_wiring.go) ; e-mails [alerts/service/assistance.go](../../internal/modules/alerts/service/assistance.go). Migration `000079_assistance_grants`.

## Modèle

`assistance_grants` est la source de vérité. Une ligne = une personne de l'équipe sur une organisation.

| Champ | Sens |
|---|---|
| `mode` | `consent` (le client accorde) ou `breakglass` (incident, sans accord) |
| `status` | `requested` → `active` → `revoked` / `expired` / `closed` ; ou `requested` → `denied` / `expired` |
| `reason`, `ticket_id` | motif (20 à 500 caractères) et ticket d'origine |
| `requested_hours`, `expires_at` | durée demandée, échéance de l'accès ouvert |
| `decided_by`, `ended_by`, `end_note` | qui a accepté, qui a clos, et pourquoi |

Une seule demande ou un seul accès ouvert à la fois par personne et par organisation (index unique partiel `assistance_grants_open`).

Un accès actif pose la liaison IAM `organizations/<id>` · `roles/support.assistant` · `user:<personne>`, avec la condition `request.time < timestamp('<échéance>')`. À l'échéance, la condition cesse d'accorder, même avant le passage du ramasseur.

## Rôle `roles/support.assistant`

Rôle **réservé** : absent de la liste des rôles attribuables, ignoré et préservé par `PolicyService.Set` (l'éditeur des accès du client ne peut ni le poser ni l'effacer). Il contient la lecture et les actions réversibles : redémarrer, revenir à la version précédente, démarrer ou arrêter un conteneur ; réactiver un déploiement de site ; arrêter ou redémarrer une base ; démarrer, arrêter ou redémarrer un VPS. Il ne contient aucune création ni suppression, ni variables, secrets, IAM, facturation ou audit, et aucune permission sensible (liste exacte : `assistantPerms` dans [roles.go](../../internal/iam/roles.go)).

Comme la liaison est posée chez le client, ses **politiques de refus s'appliquent** : un client peut interdire toute assistance.

## Permissions

| Permission | Qui | Sensible |
|---|---|---|
| `platform.assistance.request` | support, administrateurs | non |
| `platform.assistance.breakglass` | administrateurs | oui (double authentification) |
| `resourcemanager.organizations.grantAssistance` | propriétaire, `roles/iam.securityAdmin` | oui |
| `resourcemanager.organizations.revokeAssistance` | propriétaire, `roles/iam.securityAdmin` | non : retirer ne doit jamais être plus difficile qu'accorder |

## API

Client (`/organizations/:id/assistance`) :

| Méthode | Chemin | Permission | Corps |
|---|---|---|---|
| GET | `` | `resourcemanager.organizations.getIamPolicy` | → `{items}` (demandes, accès ouverts, passés) |
| POST | `/:grantID/approve` | `grantAssistance` | `{hours}` (1 à 24 ; défaut : la durée demandée) |
| POST | `/tickets/:ticketID` | `grantAssistance` | `{reason, hours}` : accès à l'agent assigné au ticket, sans attendre sa demande |
| POST | `/:grantID/deny` | `revokeAssistance` | `{note}` |
| POST | `/:grantID/revoke` | `revokeAssistance` | `{note}` ; effet immédiat (liaison retirée, cache invalidé) |

Équipe (internes, motif `X-Betorun-Justification` exigé ; l'appel entre au journal du client) :

| Méthode | Chemin | Permission | Corps |
|---|---|---|---|
| POST | `/admin/support/tickets/:ticketID/assistance` | `platform.assistance.request` | `{reason, hours}` (défaut 4 h) |
| POST | `/admin/support/tickets/:ticketID/assistance/breakglass` | `platform.assistance.breakglass` | `{reason, minutes}` (5 à 60, défaut 60) |
| GET | `/admin/assistance` | `platform.support.get` | → `{items}` : les accès de la personne seulement |
| POST | `/admin/assistance/:grantID/close` | `platform.assistance.request` | `{note}` : clore son accès ou retirer sa demande |

Erreurs : `404` (accès inconnu ou d'une autre organisation), `409` (déjà une demande ou un accès ouvert ; demande déjà tranchée ; accès plus actif), `400` (motif trop court, ticket sans agent assigné).

## Règles

- **Demande** : sans réponse, elle expire après 72 h.
- **Durée** : 4 h par défaut, 24 h au plus. Bris de glace : 1 h au plus, non renouvelable sans nouveau bris de glace.
- **E-mails** aux propriétaires (FR/EN) : demande reçue, accès ouvert, bris de glace (immédiat, avec le motif). Lien vers la page `/team-access` de la console.
- **Journal du client** : `organization.assistance.requested`, `.granted`, `.denied`, `.breakglass`, `.revoked`, `.expired`, `.closed`, `.reconciled`, avec la personne, le motif, le ticket et l'échéance. Les actions faites pendant l'accès y entrent comme pour tout membre.
- **Ramasseur** (chaque minute) : il expire les accès échus et les demandes trop anciennes, puis retire toute liaison `roles/support.assistant` sans accès actif (restauration, édition à la main ; `organization.assistance.reconciled`).

## Opérateur

Pendant un accès actif, l'Opérateur reçoit les outils `customer_*` (lectures et cartes sur les ressources du client). Voir [operator.md](operator.md#cartes-chez-un-client-accès-dassistance).

## Tests

`IAM_E2E=1 go test ./internal/modules/assistance/` sur la base jetable (`bash scripts/iam_e2e_db.sh`) :

- droits ouverts et fermés ;
- édition des accès par le client ;
- refus, retrait, échéance, bris de glace ;
- rapprochement ;
- validation concurrente (une seule passe).
