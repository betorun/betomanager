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

## Tests

- `domain/` : tailles, prix, plafond, offre gratuite, projection, validation d'une grille.
- `service/` : relevé idempotent, plus grande taille de l'heure, nouvelle version de grille, monnaie inconnue.
- `xykpay/` : signature des webhooks, client (faux serveur).
- `pdf/` : rendu d'une facture et d'un reçu (`PDF_OUT=… go test ./internal/modules/billing/pdf/` écrit l'exemple).
- `repository/` (vrai Postgres, `IAM_E2E=1` après `bash scripts/iam_e2e_db.sh`) : relevé et estimation de bout en bout, versions de grille ; grand livre équilibré et immuable ; activation, crédit d'essai, débit (crédits d'abord, idempotent), recharge par webhook signé (créditée une fois), ajustement, expiration, historique, administration ; grâce puis suspension, reprise après recharge (seuls les conteneurs arrêtés par elle), crédit d'essai une fois par numéro, budget avec arrêt puis relance, facture et reçu (numéros, TVA, PDF, immuables).

## Suite

Phase 5 : plans liés aux limites, cartes, postpayé entreprise, revente de domaines. Restent aussi : la page « compte suspendu » des sites statiques (serveur des sites à trancher), la suppression des données après 30 jours de suspension (aujourd'hui décision de l'équipe, pas automatique).
