# Module compute (VPS)

Serveurs virtuels revendus sous la marque betorun. Le fournisseur n'apparaît
jamais côté client (ni dans l'API, ni dans la console, ni dans les messages).

État : **lot 1** — fondations (offres, outbox, mode compute, client du
fournisseur) ; **lot 2** — lecture chez le fournisseur, réconciliation,
page « VPS » du back-office ; **lot 3** — commande payée par le solde,
création avec la clé SSH du client, remboursement en cas d'échec,
renouvellement mensuel et suspension. Pas encore d'action du client sur un
VPS ni de pages de la console (lot 4).

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

## Commande (lot 3)

`POST /organizations/:id/compute/vps` (`billing.payments.create`, sensible :
double authentification récente) :

1. contrôles, avant tout débit : projet de l'organisation, nom (étiquette
   DNS, unique dans le projet), clé SSH (ed25519, ecdsa, RSA ≥ 2048 bits ;
   une seule, commentaire retiré), offre vendable à cet emplacement avec ce
   système, prix annoncé = prix du moment (`VPS_PRICE_CHANGED` sinon) ;
2. **une transaction** : VPS `pending`, débit du premier mois sur le solde
   payé (référence `compute:vps:<id>:<date>`, idempotente ; jamais les
   crédits offerts ; postpayé : dans le plafond), demande `vm.create`
   (outbox). Solde insuffisant : rien n'est créé (`INSUFFICIENT_FUNDS`).

Workflow `vm.create` (mode compute) :

```
PlaceOrder (une seule tentative : jamais deux commandes payées)
  panier (1 mois) → datacenter et système (valeurs proposées par le fournisseur)
  → aperçu : prix TTC ≤ prix de l'offre, sinon rien n'est payé (price_changed)
  → paiement par le moyen par défaut du compte
OrderState toutes les minutes, 6 h au plus → identifiant du VPS (serviceName)
Ready (existe, aucune opération en cours), 2 h au plus
Rebuild : système de l'offre, clé du client, doNotSendPassword (aucun mot de passe par e-mail)
Task jusqu'à « done », 1 h au plus → running, adresses relevées
```

Tout échec : mois **remboursé** (référence `…:refund`, une seule fois), VPS
`failed` avec sa raison (`provider`, `price_changed`, `timeout`). Une
réponse perdue au paiement peut laisser un VPS payé chez le fournisseur sans
client : la réconciliation le montre en orphelin.

`KUBEMANAGER_COMPUTE_ORDERING_ENABLED=true` (sur l'API) ouvre les commandes
réelles ; sans lui, offres et prix seulement (`UNAVAILABLE`).

## Renouvellement et suspension (lot 3)

Le worker (toutes les 15 min) traite les VPS dont le mois payé est fini :

- **payé** : un mois débité au prix de la grille du moment ; il commence à
  la fin du précédent (`paid_until`) ;
- **solde insuffisant** : VPS `suspended` (raison `unpaid`) et demande
  `vm.stop` ; retenté à chaque passage ;
- **payé après suspension** : le mois commence au paiement (les mois de
  suspension ne sont pas facturés), demande `vm.start`, le VPS repart ;
- un VPS suspendu par l'équipe (autre raison) n'est jamais relancé par un paiement.

Un mois se termine le même jour du mois suivant, borné à son dernier jour
(31 janvier → 28 février).

Le fournisseur renouvelle de son côté chaque mois (paiement automatique du
compte) : un VPS suspendu reste payé chez lui jusqu'à la résiliation (lot 7).

## Lecture chez le fournisseur (lot 2)

`repository/ovhvps.Provider` implémente `ComputeProvider` en lecture :
`GET /vps` puis, pour chaque VPS (4 à la fois), `/vps/{sn}`, `/vps/{sn}/ips`
et `/vps/{sn}/tasks?state=doing|todo` ; `Task` suit une opération
(`/vps/{sn}/tasks/{id}`). Les actions (démarrer, arrêter, réinstaller,
console) rendent `ErrNotSupported` jusqu'au lot 4.

États traduits (`TranslateState`) :

| Fournisseur | Plateforme | Opération en cours |
|---|---|---|
| running | running | non |
| stopped | stopped | non |
| installing | creating | oui |
| stopping | stopped | oui |
| rebooting | running | oui |
| maintenance, rescued, upgrading, backuping, migrating, autre | — | oui |

Une tâche prévue ou en cours compte aussi comme opération en cours.

## Réconciliation (lot 2)

`service.Reconciler`, exécuté par le mode compute :

- **périodique** : workflow `ComputeReconcileWorkflow`, identifiant fixe
  `compute-reconcile`, cron `*/10 * * * *`, planifié au démarrage du mode
  compute s'il a les clés (déjà planifié : rien à faire). Pour changer la
  fréquence : arrêter `compute-reconcile` dans Temporal, puis redémarrer le
  mode compute ;
- **à la demande** : `POST /admin/compute/reconcile` dépose une demande
  `compute.reconcile` (outbox).

Règles d'une passe :

- corrigé : marche ↔ arrêt et adresses d'un VPS `running`/`stopped`, sans
  opération en cours, et seulement si sa version n'a pas bougé depuis la
  lecture (une action du client garde la main) ; la version augmente ;
- relevé, jamais corrigé en silence :
  - `orphan` : chez le fournisseur, inconnu de la plateforme (payé, vendu à personne) ;
  - `state` : état différent hors marche/arrêt (suspendu encore en marche,
    résilié encore présent…) ; un VPS en création est laissé au workflow de commande ;
  - `missing` : VPS de la plateforme absent chez le fournisseur.
- le relevé (`compute_provider_vms`) est remplacé d'un bloc ; une passe en
  échec garde le précédent. Chaque passe est enregistrée
  (`compute_reconcile_runs`), réussie ou non.

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
| GET | `/admin/compute/offers` | `platform.billingCatalogs.get` |
| PUT | `/admin/compute/offers/:code` | `platform.billingCatalogs.update` |
| GET | `/organizations/:id/compute/vps` | `billing.accounts.get` |
| GET | `/organizations/:id/compute/vps/:vpsID` | `billing.accounts.get` |
| POST | `/organizations/:id/compute/vps` | `billing.payments.create` |
| GET | `/admin/compute/vps` | `platform.compute.get` |
| POST | `/admin/compute/reconcile` | `platform.compute.manage` |
| POST | `/admin/compute/ping` | `platform.compute.manage` |
| GET | `/admin/compute/requests/:requestID` | `platform.compute.get` |

`POST /admin/compute/ping` dépose une demande `compute.ping` : elle traverse
toute la chaîne (outbox → relais → Temporal → mode compute → `done`) sans
toucher au fournisseur. Sert à vérifier un déploiement.

## Tables

- `compute_offers` : catalogue (code, plan, prix fournisseur, coefficient, arrondi, spécifications, emplacements, images).
- `compute_vps` : VPS des projets (état : pending, creating, running, stopped, suspended, failed, deleted).
- `compute_requests` : outbox (pending → started → done / failed).
- `compute_secrets` : secrets à usage unique (mot de passe initial), 15 min.
- `compute_provider_vms` : dernier relevé du fournisseur (état brut, plan, zone, adresses, écart).
- `compute_reconcile_runs` : passes de réconciliation (compteurs, erreur).

## Configuration

| Variable | Où | Rôle |
|---|---|---|
| `KUBEMANAGER_COMPUTE_ENABLED` | API, worker, compute | routes, relais |
| `KUBEMANAGER_COMPUTE_TASK_QUEUE` | worker, compute | file Temporal (défaut `compute`) |
| `KUBEMANAGER_COMPUTE_ORDERING_ENABLED` | API | commandes réelles ouvertes |
| `KUBEMANAGER_COMPUTE_OVH_*` | **compute seulement** | clés limitées à `/vps/*`, `/order/*`, `/ip/*`, `/me` |

Local : `make dev-compute` (service `kubemanager-compute`).

## Code

- `domain/` : VPS, offres, calcul du prix, demandes, secrets.
- `service/` : ports (`ComputeProvider`, `Store`, `Starter`), relais, offres.
- `repository/` : `GormStore` (Postgres) ; `repository/ovhvps` : fournisseur OVH.
- `workflows/` : `RequestWorkflow`, `ReconcileWorkflow`, activités.
- Client signé du fournisseur : `internal/infrastructure/ovh` (partagé avec les domaines).
- Câblage : `internal/infrastructure/router/compute_wiring.go`.

## Tests

```
go test ./internal/modules/compute/...
bash scripts/iam_e2e_db.sh && IAM_E2E=1 go test ./internal/modules/compute/... -count=1; docker rm -f iam-e2e-pg
# lecture réelle du compte (aucune commande) :
set -a; . ./.env; set +a; OVH_LIVE=1 go test ./internal/modules/compute/repository/ovhvps -run TestLive -v
```

Back-office : page « VPS » (`/admin/vps` dans kweb).
