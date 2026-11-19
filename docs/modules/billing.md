# Module `billing`

> Facturation prépayée. Phase 2 (en place) : l'usage réservé est mesuré et valorisé, la console affiche un **coût estimé** ; rien n'est débité.

Décisions : [ADR 0012](../adr/0012-facturation.md).

## Architecture

```
billing/
  domain/      règles pures : monnaie (Milli), grille, tailles, prix, estimation, validation
  inventory/   adaptateur en lecture seule : ce que réservent conteneurs, bases, builds, domaines
  repository/  tables du module : billing_usage, billing_accounts, billing_catalogs
  service/     cas d'usage : relevé (worker), estimation et grilles (API)
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
| GET | `/billing/catalog?currency=XAF` | authentifié | Grille en vigueur (formulaires : « ≈ 3 000 FCFA/mois ») |
| GET | `/admin/billing/catalogs` | `platform.billingCatalogs.get` | Grilles en vigueur, une par monnaie |
| PUT | `/admin/billing/catalogs/:currency` | `platform.billingCatalogs.update` (sensible) | Nouvelle version d'une grille ; validée, appliquée au relevé suivant |

Erreurs : `INVALID_INPUT` (mois), `UNKNOWN_CURRENCY` (404), `INVALID_CATALOG` (400, avec la raison).

## Tests

- `domain/` : tailles, prix, plafond, offre gratuite, projection, validation d'une grille.
- `service/` : relevé idempotent, plus grande taille de l'heure, nouvelle version de grille, monnaie inconnue.
- `repository/` (vrai Postgres, `IAM_E2E=1` après `bash scripts/iam_e2e_db.sh`) : relevé et estimation de bout en bout, versions de grille.

## Suite

Phase 3 : wallet par organisation, grand livre en partie double, recharge XYKPay, débits, alertes de solde. Voir l'ADR.
