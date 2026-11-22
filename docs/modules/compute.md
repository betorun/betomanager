# Module compute (VPS)

Serveurs virtuels revendus sous la marque betorun. Le fournisseur n'apparaît
jamais côté client (ni dans l'API, ni dans la console, ni dans les messages).

État : **lot 1** — fondations (offres, outbox, mode compute, client du
fournisseur). Pas encore de commande ni de cycle de vie des VPS (lots 2+).

## Processus

```
API (server / platform) ──tx métier + compute_requests──▶ Postgres
                                                          │
worker : Relay (2 s, FOR UPDATE SKIP LOCKED) ◀────────────┘
   │ ExecuteWorkflow(ID = request_id, RejectDuplicate)
   ▼
Temporal, file « compute » ──▶ APP_MODE=compute : RequestWorkflow + activités
                                                   │ seul process avec les clés
                                                   ▼
                                               fournisseur (API signée)
```

- **Outbox** : toute commande VPS écrit sa demande (`compute_requests`) dans la
  même transaction que son effet métier. Transaction annulée : demande annulée.
- **Relais** (`service.Relay`, dans le worker) : réclame les demandes `pending`
  (`SKIP LOCKED` : plusieurs workers ne lancent jamais deux fois la même),
  démarre le workflow puis passe la demande à `started`. Échec de démarrage :
  la demande reste `pending` et repart au passage suivant.
- **Une seule exécution** : `WorkflowID` = identifiant de la demande, politique
  `REJECT_DUPLICATE` : même terminé, il n'est jamais relancé.
- **Mode compute** (`APP_MODE=compute`) : worker Temporal de la file
  `compute.task_queue` ; seul à charger `compute.ovh.*`. L'API et le worker
  n'ont pas les clés.
- **Clôture** : l'activité `Finish` passe la demande à `done` / `failed` ;
  idempotente (une demande terminée ne change plus).

## Offres et prix

Table `compute_offers` (VPS-1 et VPS-2 créées par la migration 000057, non
vendables tant que l'équipe n'a pas posé le plan et le prix du fournisseur).

Prix mensuel prépayé, jamais écrit dans le code :

```
prix payé au fournisseur (TTC si TVA) × taux EUR du compte (655,957 en XAF/XOF)
  × coefficient de l'offre (coefficient_bp, 20000 = ×2)
  arrondi au supérieur à round_to (500 FCFA)
```

Exemple : 6,49 € × 655,957 × 2 = 8 514,3 → **9 000 FCFA**.

Une offre n'est proposée que si elle est publique, a un plan fournisseur et un
prix. Emplacements autorisés : `gra`, `rbx`, `sbg` ; images : `ubuntu-24.04`,
`debian-12` (pas de Windows ; réinstallation par clé SSH seulement).

## Routes

| Méthode | Chemin | Droit |
|---|---|---|
| GET | `/organizations/:id/compute/offers` | `billing.accounts.get` |
| GET | `/admin/compute/offers` | `platform.billing.catalogs.get` |
| PUT | `/admin/compute/offers/:code` | `platform.billing.catalogs.update` |
| POST | `/admin/compute/ping` | `platform.billing.catalogs.update` |
| GET | `/admin/compute/requests/:requestID` | `platform.billing.catalogs.get` |

`POST /admin/compute/ping` dépose une demande `compute.ping` : elle traverse
toute la chaîne (outbox → relais → Temporal → mode compute → `done`) sans
toucher au fournisseur. Sert à vérifier un déploiement.

## Tables

- `compute_offers` : catalogue (code, plan, prix fournisseur, coefficient, arrondi, spécifications, emplacements, images).
- `compute_vps` : VPS des projets (état : pending, creating, running, stopped, suspended, failed, deleted).
- `compute_requests` : outbox (pending → started → done / failed).
- `compute_secrets` : secrets à usage unique (mot de passe initial), 15 min.

## Configuration

| Variable | Où | Rôle |
|---|---|---|
| `KUBEMANAGER_COMPUTE_ENABLED` | API, worker, compute | routes, relais |
| `KUBEMANAGER_COMPUTE_TASK_QUEUE` | worker, compute | file Temporal (défaut `compute`) |
| `KUBEMANAGER_COMPUTE_OVH_*` | **compute seulement** | clés limitées à `/vps/*`, `/order/*`, `/ip/*`, `/me` |

Local : `make dev-compute` (service `kubemanager-compute`).

## Code

- `domain/` : VPS, offres, calcul du prix, demandes, secrets.
- `service/` : ports (`ComputeProvider`, `Store`, `Starter`), relais, offres.
- `repository/` : `GormStore` (Postgres).
- `workflows/` : `RequestWorkflow`, activités.
- Client signé du fournisseur : `internal/infrastructure/ovh` (partagé avec les domaines).
- Câblage : `internal/infrastructure/router/compute_wiring.go`.

## Tests

```
go test ./internal/modules/compute/...
bash scripts/iam_e2e_db.sh && IAM_E2E=1 go test ./internal/modules/compute/... -count=1; docker rm -f iam-e2e-pg
```
