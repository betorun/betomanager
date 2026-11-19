# ADR 0012 — Facturation prépayée

Statut : décidé ; phases 2 à 4 implémentées. Date : 2026-11-19.
Code : [internal/modules/billing](../../internal/modules/billing), doc : [docs/modules/billing.md](../modules/billing.md).

## Contexte

betorun héberge des développeurs d'Afrique centrale et de l'Ouest. Ils paient
surtout par mobile money : un paiement poussé, ponctuel, et non une carte qu'on
débite chaque mois. L'IAM prévoit déjà qu'une organisation porte la
facturation, et les permissions `billing.*` existent (sensibles : double
authentification récente). Arrêter un conteneur le met à zéro réplica sans le
supprimer.

## Décisions

### Modèle

1. **Prépayé.** Un **wallet par organisation** (ni par projet, ni par personne).
   Tous les projets de l'organisation y puisent ; chaque projet peut avoir un
   budget mensuel (alerte, puis arrêt). Le postpayé par carte ou virement
   viendra plus tard, pour les entreprises.
2. **XYKPay sert seulement à payer** (rail mobile money). Le solde et le grand
   livre restent dans betorun : l'hébergeur ne dépend pas de XYKPay pour savoir
   ce qu'un client doit.
3. **Une recharge solde d'abord le négatif**, puis relance seule les ressources
   suspendues. Leçon de SYK : une recharge qui ne soldait pas les impayés
   laissait l'école suspendue.
4. **Entité et monnaies.** Une seule entité de facturation pour commencer, en
   Afrique centrale ou de l'Ouest, avec la TVA de son pays. Une monnaie par
   compte, fixée à la création. XAF et XOF valent le même montant (même
   arrimage à l'euro) : une seule grille en FCFA sert les deux zones. Les autres
   monnaies (CDF…) auront leur propre grille. La facturation transfrontalière et
   la TVA sont à valider par un comptable avant la phase 4.

### Mesure et prix

5. **On facture des tailles, pas des millicœurs.** Un conteneur est classé dans
   la plus petite taille qui couvre ses limites CPU et mémoire, par réplica.
   Prix au mois, ramené à l'heure (÷ 730) et plafonné au mois : on ne paie
   jamais plus de 730 h d'une ressource.
6. **On facture ce qui est réservé**, comme les quotas : un conteneur en échec
   compte (ses pods tournent), un conteneur arrêté ne coûte que son disque.
7. **Échantillonnage de l'état réservé**, plutôt qu'une table d'événements.
   Toutes les 5 minutes, le worker relève ce qui est réservé et écrit une ligne
   par (ressource, compteur, heure), gardant le maximum vu dans l'heure. Pas
   d'événement perdu possible : ce qui est réservé à un relevé est compté. La
   clé (compteur, ressource, heure) rend le relevé idempotent : le relancer ne
   facture jamais deux fois. Une heure commencée est due.
8. **Montants entiers**, en millièmes d'unité de la monnaie (1 FCFA = 1000) :
   le prix horaire d'une petite taille (≈ 2 FCFA) garde sa précision.
   L'arrondi à l'unité ne se fait qu'à l'affichage et sur la facture.
9. **Aucun montant dans le code.** Les grilles de prix vivent en base
   (`billing_catalogs`), une par monnaie, et l'équipe de la plateforme les
   modifie depuis l'administration (`PUT /admin/billing/catalogs/:currency`,
   droit `platform.billingCatalogs.update`, double authentification récente).
   Chaque modification crée une version ; la plus récente s'applique dès le
   relevé suivant. Chaque ligne d'usage garde la version et le prix appliqués :
   changer la grille ne réécrit jamais le passé. Une grille est validée avant
   d'être enregistrée (tailles croissantes en CPU, mémoire et prix ; montants
   positifs). Une monnaie de compte sans grille retombe sur XAF, journalisé.

Grille de départ, amorcée par la migration 000034 (FCFA par mois, à recaler
pendant la phase 2 depuis l'administration) :

| Ressource | Taille / unité | Prix |
|---|---|---|
| Conteneur Mini | 0,25 vCPU / 256 Mo | 1 500 |
| Conteneur Petit | 0,5 vCPU / 512 Mo | 3 000 |
| Conteneur Moyen | 1 vCPU / 1 Go | 6 000 |
| Conteneur Grand | 2 vCPU / 2 Go | 12 000 |
| Conteneur Très grand | 4 vCPU / 4 Go | 24 000 |
| Au-delà | proportionnel au Très grand | — |
| Base de données | instance | 2 000 |
| Disque (base ou conteneur) | par Go | 300 |
| Build | par minute, au-delà de l'offre gratuite | 10 |
| Domaine personnalisé d'un site | par domaine | 0 (gratuit au début) |
| Site statique, images au registre | — | 0 (dans les limites du projet) |

Les domaines personnalisés des sites sont mesurés dès maintenant à 0 : les
rendre payants ne demandera qu'un changement de grille.

10. **Offre gratuite**, par organisation et par mois : l'équivalent d'un
    conteneur Mini, d'une base de 1 Go (instance et disque), 100 minutes de
    build ; les sites statiques sur un sous-domaine betorun. Plus, en phase 3,
    un crédit d'essai de 5 000 FCFA valable 30 jours, consommé avant l'argent
    payé. Une offre gratuite par numéro de téléphone (anti-abus).

### Argent (phases 3 et 4)

Activation **organisation par organisation** par l'équipe : la phase 2 continue
pour les autres (coût estimé), ce qui permet un pilote (Bomba) sans basculer
toute la plateforme. Le crédit d'essai, les bornes d'une recharge et la durée
de la grâce sont dans la grille (modifiables), pas dans le code.

**Phase 4, choix de mise en œuvre** : le crédit d'essai est offert à la
première recharge réussie, une fois par numéro Mobile Money : le paiement
prouve que le numéro appartient au client, sans vérification par SMS. Les prix
sont TTC ; la facture extrait la TVA du montant payé en argent (les crédits
offerts ne sont pas une recette). Un reçu de recharge ne porte pas de TVA. La
suspension n'arrête que les conteneurs : la page « compte suspendu » des sites
attend la décision sur l'hébergement des sites, et la suppression des données
après 30 jours reste une décision de l'équipe.

11. **Grand livre en partie double, immuable** : comptes wallet client,
    revenus, TVA, crédits offerts ; chaque mouvement est une écriture
    équilibrée ; on corrige par une écriture inverse. Le solde est la somme des
    écritures (instantané en cache). Les crédits offerts sont consommés avant
    l'argent payé et expirent.
12. **Recharge** : intention de paiement avec clé d'idempotence, demande
    poussée sur le téléphone, webhook signé et vérifié, puis écriture. Statut
    « en attente » explicite, expiration gérée. Réconciliation quotidienne avec
    le relevé de XYKPay.
13. **Solde bas, grâce et suspension** : alertes à 7 jours, 2 jours et zéro de
    consommation restante (e-mail, console, temps réel). Grâce de 3 jours de
    solde négatif, plafonnée à 3 jours de consommation. Puis suspension douce :
    conteneurs arrêtés, sites remplacés par une page « compte suspendu ».
    Données conservées 30 jours ; pour les bases, avertissements à J+7, J+21 et
    J+28, puis suppression, avec une dernière copie gardée encore 30 jours.
    Reprise automatique après recharge (décision 3).
14. **Factures** : numérotation séquentielle sans trou, mentions légales, TVA,
    PDF archivé non modifiable ; un reçu par recharge.
15. Toute action de facturation exige une double authentification récente
    (permissions `billing.*` sensibles, déjà en place).

## Phases

| Phase | Contenu | État |
|---|---|---|
| 0 | Ce document | fait |
| 1 | Plusieurs projets par organisation, compte de facturation par organisation | en cours (hors de ce module) |
| 2 | Mesure, valorisation, coût estimé dans la console, rien n'est débité | **implémentée** |
| 3 | Wallet, grand livre, recharge XYKPay, débits, alertes, crédit d'essai ; administration (organisations, crédits, ajustements, activation, recettes) | **implémentée** |
| 4 | Grâce et suspension, reprise, factures et reçus PDF (TVA), budgets par projet, crédit d'essai par numéro | **implémentée** |
| 5 | Plans liés aux limites, cartes, postpayé, revente de domaines | à faire |

La phase 2 tourne quelques semaines sur de vrais clients (Bomba) : elle révèle
les erreurs de mesure et cale la grille avant qu'un franc ne soit débité.

## Conséquences

- La mesure ne dépend d'aucun événement : un conteneur qui tourne est compté
  au relevé suivant, même si un événement a été perdu.
- Granularité d'une heure : une ressource créée puis supprimée en 10 minutes
  paie une heure. C'est voulu (et négligeable au prix horaire).
- Un changement de taille en cours d'heure est compté à la plus grande taille
  de l'heure.
- Les crons sont comptés comme les autres conteneurs (leur réservation est
  tenue tant qu'ils sont planifiés). À revoir en fin de phase 2 si l'écart avec
  leur usage réel pèse.
- Le module lit l'état des autres modules par un adaptateur en lecture seule
  (`billing/inventory`) ; il n'écrit que dans ses propres tables.

## Alternatives écartées

- **Table d'événements de cycle de vie** : un événement perdu fait tourner une
  ressource gratuitement ; il aurait fallu une réconciliation pour s'en
  protéger. Le relevé de l'état réservé est sa propre réconciliation.
- **Prix au millicœur** : illisible pour le client, et la marge échappe.
- **Solde tenu par XYKPay** : couple l'hébergeur à un prestataire de paiement.
- **Nombres à virgule** : erreurs d'arrondi cumulées.
