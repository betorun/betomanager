# Module `billing`

> Facturation prépayée. Phase 2 : l'usage réservé est mesuré et valorisé (coût estimé). Phase 3 : wallet par organisation, grand livre, recharges XYKPay, débit de l'usage pour les organisations dont l'équipe a activé la facturation réelle. Phase 4 : grâce puis suspension douce, reprise après recharge, factures et reçus PDF, budgets par projet, crédit d'essai une fois par numéro.

Décisions : [ADR 0012](../adr/0012-facturation.md).

## Architecture

```
billing/
  domain/      règles pures : monnaie (Milli), grille, tailles, prix, estimation, validation
  inventory/   adaptateur en lecture seule : ce que réservent conteneurs, bases, builds, domaines
  repository/  tables du module : billing_usage, billing_accounts, billing_catalogs
  service/     cas d'usage : relevé et débit (worker), estimation, wallet, recharges, administration (API)
  xykpay/      adaptateur du rail de paiement (API XYKPay, signature des webhooks)
  pdf/         rendu des factures et reçus (go-pdf/fpdf), une fois à l'émission
  billing.go   contrôleur HTTP
```

Le service ne dépend que de deux ports (`service.Inventory`, `repository.Store`) : il se teste en mémoire. L'inventaire est le seul endroit où la facturation connaît le schéma des autres modules ; elle n'écrit jamais chez eux. Câblage : [billing_wiring.go](../../internal/infrastructure/router/billing_wiring.go).

## Modèle (migration 000034)

| Table | Rôle |
|---|---|
| `billing_catalogs` | grille de prix par monnaie, une version par modification (la plus récente fait foi). Montants en unités de la monnaie, par mois ; JSON au format `domain.CatalogUpdate`. Amorcée en XAF et XOF |
| `billing_usage` | une ligne par (compteur, ressource, heure) : quantité, taille, prix unitaire, montant, version de grille. Pas de clé étrangère : l'usage d'une ressource supprimée reste dû |
| `billing_accounts` | compte d'une organisation : monnaie, pays. Absent : XAF |

**Montants** : entiers en millièmes d'unité (`domain.Milli`, 1 FCFA = 1000) ; arrondis à l'unité seulement à l'affichage.

## Mesure

Le worker relève l'état réservé toutes les 5 minutes (`Service.Run`, lancé dans [app.go](../../internal/infrastructure/router/app.go)) et l'enregistre pour l'heure en cours. Sur conflit (même compteur, ressource et heure), le plus haut montant l'emporte, avec sa taille : relancer un relevé ne compte rien deux fois.

| Compteur | Source | Quantité |
|---|---|---|
| `compute` | conteneur non supprimé, non arrêté (en échec compris) | réplicas, à la taille couvrant les limites CPU et mémoire d'un réplica |
| `disk` | conteneur avec disque (arrêté compris) | Go commencés |
| `database` / `database_disk` | base | 1 / Go du disque |
| `build_minutes` | build terminé dans les 6 dernières heures (sauf échec `INTERNAL`) | minutes commencées, dans l'heure de fin |
| `site_domain` | domaine personnalisé vérifié d'un site | 1 (prix 0 au départ) |

**Taille** : la plus petite de la grille qui couvre le réplica ; au-delà de la plus grande, `custom`, prix proportionnel à elle sur la ressource la plus demandée.

## Estimation

`Service.Estimate` : totaux du mois par ressource, chaque ressource horaire **plafonnée à 730 heures** de sa ligne la plus chère ; offre gratuite déduite au niveau de l'organisation (calcul, base, minutes de build, montants de la grille) ; **projection** de fin de mois au rythme de l'heure en cours (mois en cours seulement).

## Endpoints HTTP

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/organizations/:id/billing/estimate?month=AAAA-MM` | `billing.accounts.get` | Coût estimé : par projet, par ressource, brut, offre gratuite, total, projection |
| GET | `/billing/catalog?currency=XAF` | publique | Grille en vigueur (page Tarifs du site, formulaires : « ≈ 3 000 FCFA/mois ») |
| GET | `/admin/billing/catalogs` | `platform.billingCatalogs.get` | Grilles en vigueur, une par monnaie |
| PUT | `/admin/billing/catalogs/:currency` | `platform.billingCatalogs.update` (sensible) | Nouvelle version d'une grille ; validée, appliquée au relevé suivant |

Erreurs : `INVALID_INPUT` (mois), `UNKNOWN_CURRENCY` (404), `INVALID_CATALOG` (400, avec la raison).

## Wallet et grand livre (phase 3, migration 000035)

| Table | Rôle |
|---|---|
| `billing_transactions` / `billing_ledger_entries` | grand livre en partie double. Une transaction (référence unique = idempotence) regroupe des écritures signées dont la somme est nulle ; **la base le vérifie au COMMIT** (trigger différé) et refuse toute modification, suppression ou TRUNCATE |
| `billing_credit_grants` | crédits offerts par lot : montant, reste, expiration ; consommés avant l'argent, le plus proche de l'expiration d'abord |
| `billing_topups` | recharges mobile money : `pending` → `succeeded` / `failed` / `expired` |
| `billing_accounts.charging` | facturation réelle activée par l'équipe, organisation par organisation ; `alert_level` : dernier seuil de solde prévenu |

**Comptes** : `customer:<org>:cash` (argent payé, peut devenir négatif), `customer:<org>:credit` (crédits offerts), `platform:revenue`, `platform:promotions`, `platform:xykpay`, `platform:adjustments`.

**Verrou** : tout mouvement qui touche un client passe sous un verrou consultatif propre à son organisation, le temps de la transaction SQL.

**Débit** (`Service.Charge`, worker, toutes les 5 minutes) : pour chaque organisation activée, coût net du mois (plafonds et offre gratuite compris) jusqu'à la dernière heure terminée, depuis l'activation, moins ce qui a déjà été débité pour ce mois. C'est une différence : un relevé tardif est rattrapé à l'heure suivante, et la référence `usage:<org>:<heure>` interdit le double débit.

**Recharge** : `POST /organizations/:id/billing/topups` crée la recharge, puis le paiement chez XYKPay (clé d'idempotence = identifiant de la recharge). Le webhook `POST /billing/webhooks/xykpay` (signature `XYK-Signature`, 5 min de tolérance) ne fait que déclencher une **relecture du paiement** ; le worker relit aussi les recharges en attente (webhook perdu). Sans réponse au bout de 15 min, la recharge expire ; elle est encore créditée si le paiement réussit ensuite. Une recharge réussie est créditée une seule fois.

**Alertes** : quand le solde passe sous 7 jours, 2 jours ou zéro de consommation, un e-mail part aux owners de l'organisation (module alerts), une fois par seuil.

**Crédit d'essai** : offert à la **première recharge réussie** de l'organisation, une seule fois par numéro Mobile Money (table `billing_trials`) : le paiement prouve que le numéro appartient au client, sans SMS à vérifier. Plus offert à l'activation (phase 4).

**Configuration** (aucun montant) : `billing.xykpay.base_url`, `api_key`, `webhook_secret` (`KUBEMANAGER_BILLING_XYKPAY_*`). Incomplète : recharges fermées.

### Endpoints (phase 3)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/organizations/:id/billing/wallet` | `billing.accounts.get` | Solde, crédits, consommation par jour, jours restants, état |
| GET | `/organizations/:id/billing/transactions` | `billing.accounts.get` | Historique (débits d'usage réunis par jour) |
| GET / POST | `/organizations/:id/billing/topups` | get / `billing.payments.create` (sensible) | Recharges récentes / nouvelle recharge |
| GET | `/organizations/:id/billing/topups/:topupID` | `billing.accounts.get` | Une recharge (relue si elle attend) |
| POST | `/billing/webhooks/xykpay` | publique, signée | Signal de paiement |
| GET | `/admin/billing/organizations?q=` | `platform.billingAccounts.get` | Recherche d'organisations |
| GET | `/admin/billing/organizations/:id` | `platform.billingAccounts.get` | Fiche : projets, wallet, coût du mois, mouvements, recharges |
| POST | `/admin/billing/organizations/:id/credits` | `platform.billingAccounts.update` (sensible) | Offrir un crédit (montant, jours, motif) |
| POST | `/admin/billing/organizations/:id/adjustments` | idem | Corriger l'argent (signé, motif) |
| PUT | `/admin/billing/organizations/:id/charging` | idem | Activer / suspendre la facturation réelle |
| GET | `/admin/billing/revenue?days=` | `platform.billingAccounts.get` | Recettes par jour et par monnaie |

## Phase 4 (migration 000036)

**Grâce et suspension douce** (`settleAccount`, après chaque débit, recharge, crédit ou ajustement) :
- solde ≤ 0 : la grâce commence (`grace_started_at`) ;
- grâce écoulée (`grace_days` de la grille, 3 par défaut) **ou** dette ≥ `grace_days` jours de consommation : suspension. Les conteneurs en marche de l'organisation sont arrêtés (`WorkloadService.Stop`), et ceux-là seulement sont retenus dans `billing_stopped_workloads` ; disques et bases restent ;
- solde > 0 : fin de la grâce, et **reprise automatique** : seuls les conteneurs arrêtés par la suspension redémarrent ;
- **garde** : `WorkloadService.Billing.CanRun` refuse déploiement, mise à jour, démarrage et retour arrière pendant une suspension (402 `ACCOUNT_SUSPENDED`) ;
- suspendre la facturation d'une organisation suspendue la relance.

**Budgets par projet** (`billing_project_budgets`) : montant mensuel, alerte e-mail aux owners du projet à 80 % puis 100 % (une fois par seuil et par mois). Avec arrêt : les conteneurs du projet s'arrêtent à 100 % (raison `budget`) et redémarrent quand le budget est relevé ou retiré, ou au mois suivant. La consommation comptée est le coût du projet avant l'offre gratuite (qui est celle de l'organisation).

**Factures et reçus** (`billing_documents`, immuables, PDF archivé tel qu'émis) :
- numérotation **sans trou** par préfixe et par année (`billing_number_counters`, pris sous verrou dans la transaction qui écrit le document) ;
- **facture mensuelle** le 1er du mois, 2 h après minuit UTC, pour chaque organisation débitée le mois écoulé : lignes par nature de ressource, offre gratuite, part réglée par les crédits offerts, net réglé par le solde. Prix **TTC** ; la TVA (`billing.issuer.vat_rate_bp`) est extraite du net payé en argent et passe du compte `platform:revenue` à `platform:vat` ;
- **reçu** pour chaque recharge réussie (sans TVA : la TVA est due à la consommation, sur la facture) ;
- profil de facturation du client (raison sociale, numéro fiscal, adresse, pays) : `PUT /organizations/:id/billing/profile` ; s'applique aux prochaines factures ;
- sans entité configurée (`billing.issuer.name` et `address`), rien n'est émis.

**Configuration** : `KUBEMANAGER_BILLING_ISSUER_{NAME,ADDRESS,TAX_ID,COUNTRY,VAT_RATE_BP,INVOICE_PREFIX,RECEIPT_PREFIX}`.

### Endpoints (phase 4)

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET / PUT | `/organizations/:id/billing/profile` | `billing.accounts.get` / `billing.accounts.update` (sensible) | Informations de facturation |
| GET | `/organizations/:id/billing/documents` | `billing.accounts.get` | Factures et reçus |
| GET | `/organizations/:id/billing/documents/:documentID/pdf` | `billing.accounts.get` | PDF tel qu'émis |
| GET / PUT | `/projects/:id/billing/budget` | `billing.accounts.get` / `billing.accounts.update` | Budget mensuel du projet (0 : retiré) |

## Plans (phase 5, migration 000037)

Un **plan** fixe les limites de **chaque projet** de l'organisation (CPU, mémoire, stockage) et un **abonnement mensuel** par monnaie. Table `billing_plans`, modifiable par l'équipe (`PUT /admin/billing/plans/:planID`, droit `platform.billingCatalogs.update`) ; amorcée avec Gratuit (limites par défaut des projets, 0), Starter et Pro. `billing_accounts.plan_id` (défaut `free`).

- **Abonnement mesuré comme une ressource** (compteur `plan`, réservation de l'organisation, projet nul) : à l'heure, plafonné au mois, hors offre gratuite ; il apparaît dans l'estimation, le débit et la facture (« Abonnement »).
- **Changer de plan** (`PUT /organizations/:id/billing/plan`) : contrôle de **tous** les projets d'abord (réservation selon les règles des quotas) ; un projet au-dessus des nouvelles limites refuse le changement (`PLAN_BELOW_USAGE`, projets nommés), rien n'est appliqué à moitié. Puis les limites de chaque projet passent par le service des projets (base, puis quota Kubernetes).
- Un plan payant demande la facturation réelle active (`PLAN_NEEDS_BILLING`) ; un plan non public n'est attribué que par l'équipe (`PUT /admin/billing/organizations/:id/plan`).
- **Le plan fait foi** : un **rapprochement** (`ReconcileLimits`, au changement de plan, à la modification d'un plan, et à chaque passage du worker) ramène chaque projet aux limites de sa cible. Un plan modifié s'applique donc aussitôt à toutes les organisations qui l'ont ; un nouveau projet reçoit les limites du plan de son organisation au plus tard au passage suivant (5 min).
- **Exceptions** : des limites posées à la main par l'équipe (page « Limites des projets », `PATCH /admin/projects/:id/limits`) sont retenues dans `billing_limit_overrides` (migration 000038) ; le rapprochement les respecte. `DELETE /admin/billing/projects/:id/limits-override` ramène le projet au plan.
- Un projet qui réserve déjà plus que sa cible garde ses limites : la modification d'un plan le rend dans `not_applied`, et le rapprochement réessaie à chaque passage.

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/billing/plans?currency=` | publique | Plans proposés (page Tarifs) |
| GET / PUT | `/organizations/:id/billing/plan` | `billing.accounts.get` / `billing.accounts.update` (sensible) | Plan de l'organisation / changer de plan |
| GET / PUT | `/admin/billing/plans[/:planID]` | `platform.billingCatalogs.get` / `update` | Catalogue des plans |
| PUT | `/admin/billing/organizations/:id/plan` | `platform.billingAccounts.update` | Attribuer un plan |
| DELETE | `/admin/billing/projects/:id/limits-override` | `platform.projects.setLimits` | Retirer l'exception : retour aux limites du plan |

## Postpayé (phase 5, migration 000039)

Pour les entreprises, activé par l'équipe (`PUT /admin/billing/organizations/:id/postpaid`) ; active aussi la facturation réelle.

- L'usage est débité à l'heure comme en prépayé : le solde devient une **dette**, tolérée jusqu'au **plafond de crédit** (défaut de la grille : `postpaid_credit_limit`, 200 000 ; réglable par client). Au-delà : grâce puis suspension, comme un solde épuisé.
- Chaque facture mensuelle porte une **échéance** (`postpaid_terms_days`, 30 j ; réglable par client) et les **instructions de paiement** (`billing.issuer.payment_instructions`).
- **Impayés** : les paiements soldent les factures de la plus ancienne à la plus récente (`domain.Receivables`, calculé à partir de la dette moins l'usage pas encore facturé). Relance e-mail une fois par facture à `postpaid_reminder_days` (7 j) après l'échéance (`billing_dunning`) ; suspension douce à `postpaid_suspend_days` (15 j), levée dès que la dette redescend.
- **Paiements** : virement enregistré par l'équipe (`POST /admin/billing/organizations/:id/payments`, compte `platform:bank`), ou recharge mobile money par le client.

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| PUT | `/admin/billing/organizations/:id/postpaid` | `platform.billingAccounts.update` | Postpayé (plafond, échéance) ou prépayé |
| POST | `/admin/billing/organizations/:id/payments` | `platform.billingAccounts.update` | Virement reçu |

Le wallet (`GET …/billing/wallet`) porte alors `postpaid` : plafond, échéance, dette, disponible, factures à payer.

## Organisation suspendue : sites et suppression des données (migration 000041)

**Sites** : vue `edge_suspended_projects` (projets des organisations facturées et suspendues, rien d'autre). `static-edge` la lit avec son rôle en lecture seule (`GRANT SELECT … edge_suspended_projects TO edge_ro`) et sert une page neutre « Site temporairement indisponible » (503, `Retry-After: 3600`, `noindex`). Le cache de l'edge (TTL court) retarde au plus d'autant la bascule.

**Suppression** (`service/purge.go`, boucle horaire `RunPurge` dans le **processus API**, une seule instance) :
- `purge_days` de la grille (30 ; 0 : jamais ; minimum 14) ;
- étapes dans `billing_accounts.purge_stage` : 1 à J+7, 2 à J-7, 3 à J-2 (un e-mail par étape, celui de l'étape atteinte seulement), 4 suppression lancée ; `purge_noticed_at` : la suppression attend au moins deux jours après le dernier avertissement envoyé (avertissements manqués, délai raccourci) ;
- `orgPurger` (`router/billing_purge_wiring.go`) : conteneurs d'abord (`WorkloadService.Delete`, disques compris), puis chaque base par `databases.Service.ArchiveAndDelete`, puis les sites (`DeleteSite`). Un échec : reprise au passage suivant, pas de `data_purged_at` ni d'e-mail « supprimé » ;
- **copie finale des bases** : Job pg_dump (`CNPG.Dump`) envoyé à l'API par URL signée, rangé sous `database-archives/<org>/<base>/…pgc`, ligne dans `database_archives` (expire `purge_days` jours après). Sans copie arrivée et enregistrée, la base n'est pas supprimée. Limite de taille : celle des imports (`MaxDumpBytes`). Copies expirées effacées chaque jour ;
- **restauration par l'équipe** : retrouver la ligne (`SELECT * FROM database_archives WHERE organization_id = …`), télécharger l'objet, `pg_restore` dans une nouvelle base (ou l'importer depuis la console) ;
- **reprise** (`SetSuspended(nil)`) : `purge_stage`, `purge_noticed_at`, `data_purged_at` remis à zéro.

Gardé : organisation, projets, grand livre, factures, domaines achetés.

## Tests

- `domain/` : tailles, prix, plafond, offre gratuite, projection, validation d'une grille.
- `service/` : relevé idempotent, plus grande taille de l'heure, nouvelle version de grille, monnaie inconnue.
- `xykpay/` : signature des webhooks, client (faux serveur).
- `pdf/` : rendu d'une facture et d'un reçu (`PDF_OUT=… go test ./internal/modules/billing/pdf/` écrit l'exemple).
- `repository/` (vrai Postgres, `IAM_E2E=1` après `bash scripts/iam_e2e_db.sh`) : relevé et estimation de bout en bout, versions de grille ; grand livre équilibré et immuable ; activation, crédit d'essai, débit (crédits d'abord, idempotent), recharge par webhook signé (créditée une fois), ajustement, expiration, historique, administration ; grâce puis suspension, reprise après recharge (seuls les conteneurs arrêtés par elle), crédit d'essai une fois par numéro, budget avec arrêt puis relance, facture et reçu (numéros, TVA, PDF, immuables) ; plans (offre publique, plan payant sans facturation refusé, limites appliquées à tous les projets, abonnement mesuré, descente refusée sans rien appliquer, nouveau projet aligné, plan modifié appliqué aussitôt avec les projets en retard signalés, exception respectée puis retirée) ; postpayé (dette sous le plafond, facture à échéance, suspension après le retard, virement qui solde et relance) ; suppression après suspension (vue de l'edge, avertissements une fois chacun, échec repris, copies gardées `purge_days` jours, jamais sans dernier avertissement depuis deux jours, reprise qui remet à zéro).
- `databases/archive_test.go` : copie finale puis suppression ; copie impossible ou non enregistrée : la base reste ; copies expirées effacées.
- `internal/edge/server_test.go` : site suspendu → 503 neutre.

## Suite

Phase 5 (suite) : cartes (Stripe, plus tard), revente de domaines (OVH : vérification de disponibilité validée sur le vrai compte ; achat à coder).
