# Module `domainsales`

> Revente de noms de domaine (ADR 0012, phase 5) : le client achète depuis la console, payé par son solde ; la plateforme commande chez le registraire (OVH) ; le client en est le titulaire. **Marque blanche** : rien ne montre le registraire au client.

## Architecture

```
domainsales/
  domain/      commande, titulaire, états, erreurs (aucune dépendance)
  repository/  domain_orders (migration 000040)
  service/     recherche, achat, traitement en tâche de fond, renouvellement, branchement
  domainsales.go  contrôleur HTTP
internal/domains/registrar/   client OVH (Check, Register, OrderStatus, Info, SetAutoRenew, SetRecords)
```

Ports du service : `registrar.Registrar`, `Wallet` (module billing : grille, débit, remboursement), `Workloads` (domaines des conteneurs). Câblage : [domainsales_wiring.go](../../internal/infrastructure/router/domainsales_wiring.go).

## Prix

Prix de vente = prix du registraire (centimes d'euro) × `eur_rate_milli` (655957, parité fixe du FCFA), + `domain_margin_bp` (2000 = 20 %), arrondi au palier `domain_round_to` (500). Réglages de la grille de prix, modifiables par l'équipe. Exemple : 8,49 € → 7 000 FCFA. Le client ne voit que le prix final.

## Parcours

1. **Recherche** : disponibilité et prix (premium refusés).
2. **Achat** : le prix annoncé doit être celui du moment (`DOMAIN_PRICE_CHANGED`) ; le solde **payé** est débité (jamais les crédits offerts ; postpayé : dans le plafond), puis la commande est créée (`ordering`).
3. **Worker** (chaque minute) : prix revérifié (s'il dépasse ce que le client a payé : remboursé, rien commandé) ; commande OVH (titulaire = contact créé pour le client, paiement par le moyen par défaut du compte) ; suivi (`placed` → `active`, ou échec → **remboursement automatique**). Jamais commandé deux fois.
4. **Renouvellement** : 30 jours avant l'expiration, le prix de renouvellement est débité ; solde insuffisant → le registraire est prévenu de ne pas renouveler (le domaine expirera), le client voit « renouvellement non payé ».
5. **Branchement** : sur un conteneur web (et `www`) : `AddDomain` du module workloads, puis écriture des enregistrements dans la zone (remplace le parking du registraire), vérification.

## Marque blanche

- Une erreur du registraire n'est jamais renvoyée : journalisée, le client reçoit `DOMAIN_SERVICE_UNAVAILABLE` (« le service des noms de domaine ne répond pas »).
- L'onglet Domaines d'un conteneur montre « DNS gérés par betorun » pour un domaine vendu (`ManagedZone`), jamais le guide du fournisseur DNS.
- Aucun texte de la console ni de l'API ne nomme le registraire.
- Visible hors de betorun, par nature : le registraire dans le WHOIS public, les serveurs de noms de la zone, et l'e-mail de vérification ICANN que le registre envoie au titulaire.

## Configuration

`KUBEMANAGER_DOMAINS_OVH_{ENDPOINT,APPLICATION_KEY,APPLICATION_SECRET,CONSUMER_KEY,SUBSIDIARY}` ; droits de la clé : `/order/*`, `/domain/*`, `POST /me/contact`, `GET /me/order/*`. `KUBEMANAGER_DOMAINS_OVH_ORDERING_ENABLED=true` ouvre les achats réels (sinon recherche et prix seulement).

## Endpoints

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/organizations/:id/domains` | `billing.accounts.get` | Domaines de l'organisation |
| GET | `/organizations/:id/domains/search?domain=` | `billing.accounts.get` | Disponibilité et prix |
| POST | `/organizations/:id/domains` | `billing.payments.create` (sensible) | Acheter |
| PUT | `/organizations/:id/domains/:orderID/auto-renew` | `billing.accounts.update` | Renouvellement annuel |
| POST | `/organizations/:id/domains/:orderID/attach` | `billing.accounts.update` | Brancher sur un conteneur (DNS automatiques) |

## Tests

- `registrar/` : signature, disponibilité, parcours de commande complet (titulaire, format du téléphone, paiement automatique), suivi, expiration, renouvellement, remplacement du parking DNS (faux serveur). Essai réel de disponibilité : `OVH_LIVE=1` (voir `ovh_live_test.go`).
- `service/` (vrai Postgres, faux registraire, vraie facturation) : prix, refus sans solde, prix obsolète, débit, commande unique, activation, DNS écrits, renouvellement coupé puis rétabli, débit unique par période, échec remboursé.
