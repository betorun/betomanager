# Module compute (VPS)

Serveurs virtuels revendus sous la marque betorun. Le fournisseur n'apparaît
jamais côté client (ni dans l'API, ni dans la console, ni dans les messages).

État : **lot 1** — fondations (offres, outbox, mode compute, client du
fournisseur) ; **lot 2** — lecture chez le fournisseur, réconciliation,
page « VPS » du back-office ; **lot 3** — commande payée par le solde,
création avec la clé SSH du client, remboursement en cas d'échec,
renouvellement mensuel et suspension ; **lot 4** — actions du client (une
opération à la fois), permissions `compute.vps.*`, temps réel, suspension
de l'organisation, pages de la console ; **lot 5** — sauvegardes
automatiques (restauration complète ou montage), option snapshot payante,
annulation des snapshots bloqués ; **lot 6** — mode secours, mot de passe
intercepté dans la boîte technique, chiffré et affiché une seule fois ;
**lot 7** — DNS inverse, blocage anti-spam (déblocage par l'équipe), port 25
sortant fermé par défaut, résiliation ; **lot 8** — stock du fournisseur,
quotas et anti-fraude, procédure abus, e-mails du fournisseur pour
l'équipe, rattachement d'un VPS livré. La console du serveur (terminal)
viendra plus tard derrière le domaine de la plateforme : l'adresse du
fournisseur n'est jamais donnée au client.

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
  la demande reste `pending` et repart au passage suivant. Chaque lancement est
  borné à 5 s et un passage à 30 s (la transaction qui verrouille les demandes
  reste courte) ; Temporal injoignable, les passages s'espacent (×2, jusqu'à
  1 min) puis reprennent leur rythme de 2 s.
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
   `private_network: true` : réseau privé du projet possible pour ce VPS
   (`VPC_UNAVAILABLE`, `VPC_REGION_MISMATCH` sinon ; voir [module vpc](vpc.md)) ;
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
Rebuild : système de l'offre, clé du client, doNotSendPassword (aucun mot de passe par e-mail),
  script du premier démarrage : port 25 fermé, puis inscription au réseau privé si demandée
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

## Actions du client (lot 4)

| Action | Depuis | Opération | Permission |
|---|---|---|---|
| démarrer | stopped | starting | `compute.vps.start` (développeur) |
| arrêter | running | stopping | `compute.vps.stop` (développeur) |
| redémarrer | running | rebooting | `compute.vps.reboot` (développeur) |
| réinstaller (système de l'offre + clé SSH ; **efface le disque**) | running, stopped | reinstalling | `compute.vps.reinstall` (éditeur, sensible) |

Lecture : `compute.vps.list`, `compute.vps.get` (lecteur). Commande :
`billing.payments.create`. Les actions et la fiche se vérifient sur le **projet** du VPS (un droit sur l'organisation est hérité) ; la liste et la commande, sur l'organisation.

**Une opération à la fois** : l'action dépose sa demande (outbox) et marque
`operation` dans la même transaction ; un index unique partiel
(`uq_compute_requests_active_vps`, migration 000060) refuse toute seconde
demande active du même VPS (`VPS_BUSY`), qu'elle vienne du client, du
renouvellement ou de la suspension. La fin de la demande (réussie ou non)
efface `operation`. Refus : `VPS_NOT_READY` (état), `VPS_SUSPENDED`.

**Temps réel** : à chaque changement, `{type: "vps.updated", vps}` sur
`project:<projet>:vps:<vps>` et `project:<projet>:events` (abonnement
réservé aux membres du projet) ; `version` permet d'ignorer un événement
plus ancien.

## Suspension de l'organisation (lot 4)

Le module billing appelle `SuspendOrganization` quand il suspend une
organisation (solde épuisé, grâce finie) et `ResumeOrganization` à la levée :

- suspension : VPS en marche ou arrêtés → `suspended` (`org_suspended`),
  état retenu (`resume_state`), arrêt demandé pour ceux en marche ; un VPS
  occupé est suspendu quand même et son arrêt est déposé à la fin de
  l'opération en cours (`StopIfSuspended`) ;
- reprise : mois encore payé → il retrouve son état (relancé s'il
  tournait) ; mois fini pendant la suspension → reste suspendu pour impayé,
  le renouvellement le relancera après paiement.

## Sauvegardes et snapshot (lot 5)

**Relevé** : l'API n'a pas les clés du fournisseur ; le mode compute relève
les sauvegardes de chaque VPS (options, points de restauration disponibles,
sauvegardes montées, snapshot) à chaque réconciliation et après chaque
opération, dans `compute_vps_backups`. `GET …/vps/:id/backups` lit ce relevé
(`refreshed_at`). Une sauvegarde montée n'expose que son disque dans le VPS :
les accès réseau du fournisseur ne sont jamais gardés. La console est
prévenue (`vps.backups.updated`) seulement si le relevé change.

**Sauvegardes automatiques** : incluses dans les offres (aucune facturation),
montrées si le fournisseur les active sur le VPS (`BACKUPS_UNAVAILABLE` sinon).

- `full` : le disque est remplacé par le point choisi (`compute.vps.restore`, sensible) ;
- `mount` : le point est attaché en disque supplémentaire, le VPS tourne ;
  `unmount` le détache.

**Option snapshot** (un seul snapshot à la fois) :

- prix : prix payé au fournisseur (`snapshot_price_cents`, TTC) × taux ×
  coefficient de l'offre, arrondi comme l'offre ; réglé par l'équipe
  (`snapshot_plan_code`, vide : non vendue) ;
- souscription (`billing.payments.create`) : un mois complet débité tout de
  suite, puis renouvelé **avec le VPS**, dans la même transaction (un solde
  qui ne couvre pas les deux suspend le VPS) ; commande de l'option chez le
  fournisseur (une tentative, prix contrôlé), échec : remboursé ;
- créer / supprimer (`compute.vps.snapshot`), revenir (`compute.vps.restore`, sensible) ;
- résiliation : par l'API des services du fournisseur (service du VPS → son option → résiliation), repli sur l'ancienne route du VPS si elle ne la trouve pas ; le snapshot est supprimé, le mois
  entamé n'est pas remboursé.

**Snapshots bloqués** : pendant la réconciliation, une opération de snapshot
ou de restauration en cours depuis plus de 6 h est annulée (`abortSnapshot`).

## Mode secours (lot 6)

Le fournisseur ne donne le mot de passe du système de secours que **par
e-mail**, au compte (boîte technique du lot 0). Il n'atteint jamais le client :

```
POST …/vps/:id/rescue (compute.vps.rescue, sensible : double authentification récente)
  → demande vm.rescue.enter (outbox), opération entering_rescue, heure de la demande retenue
mode compute : démarrage « rescue » (netbootMode), redémarrage, rescue = active, secret = waiting
  → boîte IMAP lue toutes les 30 s, 15 min au plus : e-mail de secours qui cite ce VPS,
    reçu après la demande (le plus récent) → identifiants chiffrés (compute_secrets,
    clé des secrets de la plateforme, liés au VPS), e-mail effacé, secret = ready
  → rien après 15 min : secret = missing, journal d'erreur, VPS signalé dans le back-office
POST …/vps/:id/rescue/password → identifiants une seule fois (lecture et marquage dans
  la même requête : deux lectures simultanées n'en obtiennent qu'une), expirés après 1 h
POST …/vps/:id/rescue/exit → démarrage sur le disque, secret effacé
```

- Le mot de passe ne passe jamais par un résultat d'activité, l'historique
  Temporal ni un journal : l'activité qui lit l'e-mail le chiffre elle-même.
- En secours, les autres actions sont refusées (`VPS_IN_RESCUE`).
- Lecture de l'e-mail tolérante (français, anglais, HTML) ; utilisateur `root` par défaut.
- Back-office : bouton « Tester la boîte e-mail » (demande `compute.mailbox`).

## Réseau et fin de service (lot 7)

- **DNS inverse** (`compute.vps.network`) : `PUT …/vps/:id/reverse` ; le nom
  doit déjà pointer vers l'adresse (vérifié par l'API avant la demande,
  `REVERSE_NOT_POINTING` sinon, puis par le fournisseur).
- **Anti-spam** : le fournisseur bloque une IPv4 qui envoie du spam ; la
  réconciliation relève le blocage (`spam_blocked_at`), le client le voit,
  **l'équipe seule** le lève (back-office, après vérification ; un nouveau
  blocage dure plus longtemps chez le fournisseur).
- **Port 25 sortant fermé par défaut** : le pare-feu du fournisseur ne filtre
  que l'entrant ; à chaque installation et réinstallation, un script
  (`domain/smtp_block.sh`, `postInstallScript`) installe une règle nftables
  (repli iptables) et un service systemd qui la repose au démarrage. L'équipe
  l'ouvre par VPS (`smtp_allowed`, back-office) : plus de règle à la prochaine
  installation ; sur l'installation en cours, le client la retire (commandes
  dans sa fiche). Le client étant root, la règle est un réglage par défaut,
  pas une garantie : le blocage anti-spam du fournisseur reste le filet.
- **Résiliation** (`compute.vps.delete`, sensible, confirmation par le nom) :
  VPS `deleted` tout de suite côté plateforme (plus listé, plus renouvelé, nom
  libéré, option snapshot et secours retirés) ; chez le fournisseur : arrêt,
  puis renouvellement coupé (`serviceInfos` : `automatic=false`,
  `deleteAtExpiration=true`) — supprimé à la fin du mois déjà payé. Pas de
  remboursement. La purge d'une organisation après une longue suspension
  résilie aussi ses VPS.

## Garde-fous avant le premier client (lot 8)

**Stock** : `GET /vps/order/rule/datacenter` (plan de l'offre) relevé à chaque
réconciliation dans `compute_stock` (statut Linux de chaque datacenter, le
meilleur par emplacement de la plateforme). Les offres montrent le stock par
emplacement (`stock`) ; une commande en rupture est refusée avant tout débit
(`OUT_OF_STOCK`) ; la précommande (délai annoncé) reste possible. Une rupture
survenue entre le relevé et la commande est remboursée (lot 3).

**Quotas et comptes récents** (réglés dans le back-office, `compute_limits`) :
VPS au plus par organisation (5) ; un compte de moins de 30 jours ou sans
recharge payée : 1 VPS, VPS-1 seulement. L'équipe accorde un plafond propre
à une organisation ou la dispense des limites des comptes récents
(`compute_org_limits`). Refus : `VPS_QUOTA_REACHED`, `NEW_ACCOUNT_LIMIT`.

**Procédure abus** : l'équipe suspend un VPS (arrêt immédiat, motif montré au
client, plus facturé pendant la suspension), le rétablit, ou le résilie en
cas de récidive grave. Le client ne peut pas relancer un VPS suspendu pour abus.

**E-mails du fournisseur** : à chaque réconciliation, les e-mails de la boîte
technique (hors mode secours) sont rangés pour l'équipe
(`compute_provider_mails`) — abus, incident ou autre, rattachés au VPS qu'ils
citent (identifiant ou adresse) — puis effacés de la boîte en une session.
Lecture bornée : les 100 plus récents, 512 Ko par e-mail. Chaque e-mail porte une
empreinte (`fingerprint`, unique) : un effacement raté ne crée pas de doublon à
la passe suivante. Page du back-office : liste, « traité ».

**VPS livré sans identifiant** : si le détail de la commande ne nomme pas le
VPS, il est rattaché au seul VPS du compte inconnu de la plateforme apparu
après la commande (`first_seen_at` du relevé) ; plusieurs candidats : l'équipe
le rattache à un orphelin depuis le back-office ; la création reprend seule.
Seuls les VPS du compte non rattachés sont lus (`GET /vps` puis chacun), pas le
parc entier ; le sondage de la livraison s'espace de 1 à 10 min (×1,5).

## Vérification des appels au fournisseur

Chaque appel de `repository/ovhvps` a été confronté au schéma officiel de
l'API (méthode, chemin, champs envoyés et obligatoires, champs lus, valeurs
d'énumération) : tous existent. Statuts à surveiller chez le fournisseur :

| Appel | Statut | Remarque |
|---|---|---|
| `POST /vps/{sn}/rebuild`, `GET /vps/{sn}/images/available[/{id}]` | bêta | seule réinstallation non obsolète (`/reinstall` est obsolète) |
| `POST /order/cartServiceOption/vps/{sn}` | bêta | seule commande d'option d'un VPS |
| `GET /services`, `GET /services/{id}/options`, `DELETE /services/{id}` | bêta | résiliation de l'option snapshot |
| `DELETE /vps/{sn}/option/{option}` | obsolète | repli seulement, si l'API des services ne trouve pas l'option |

Ce que le schéma ne peut pas prouver, à confirmer par l'essai réel
(`OVH_LIVE=1`, lot 0) : les libellés et valeurs de configuration du panier
(`vps_datacenter`, `vps_os`), les plans des offres et de l'option snapshot,
le format de l'e-mail du mode secours.

## Lecture chez le fournisseur (lot 2)

`repository/ovhvps.Provider` implémente `ComputeProvider` en lecture :
`GET /vps` puis, pour chaque VPS (4 à la fois), `/vps/{sn}`, `/vps/{sn}/ips`
et `/vps/{sn}/tasks?state=doing|todo` ; un VPS illisible est rendu
« illisible » (`Unread`) sans faire échouer la liste (tous illisibles : échec).
`Task` suit une opération
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
| maintenance, rescued, upgrading, backuping, autre | — | oui |

Une tâche prévue ou en cours compte aussi comme opération en cours.

Client OVH (`internal/infrastructure/ovh`) : débit plafonné par process
(10 appels/s, rafale 20), connexions gardées (16 par hôte) ; une lecture
refusée en 429/502/503/504 est reprise deux fois (`Retry-After` respecté,
borné à 10 s), une écriture jamais.

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
  échec garde le précédent. Un VPS illisible garde sa ligne précédente et
  n'est pas compté absent (`unread` de la passe). Une écriture en échec sur un
  VPS n'arrête pas la passe (erreur relevée à la fin). Chaque passe est
  enregistrée (`compute_reconcile_runs`), réussie ou non, même annulée
  (clôture sur un contexte détaché) ;
- **une seule passe à la fois** (`BeginRun` : verrou de transaction + passe
  non close de moins de 30 min) : une passe manuelle pendant la périodique
  finit en `failed` « déjà en cours », sans reprise ;
- signe de vie Temporal (`HeartbeatTimeout` 2 min) : une passe bloquée est
  reprise sans attendre les 10 min ;
- sauvegardes relevées une fois par heure (chaque VPS sur une des six passes
  de l'heure), anti-spam à chaque passe, 4 VPS à la fois ;
- **purge** à chaque passe (lots de 1 000) : secrets lus ou expirés, demandes
  closes depuis plus de 90 jours, passes de plus de 30 jours.

## Offres et prix

Table `compute_offers` (VPS-1 et VPS-2 créées par la migration 000057, non
vendables tant que l'équipe n'a pas posé le plan et le prix du fournisseur).

Prix mensuel prépayé, jamais écrit dans le code :

```
prix payé au fournisseur (TTC si TVA) × taux EUR de la grille du compte (655,957 en XAF/XOF)
  × coefficient de l'offre (coefficient_bp, 20000 = ×2)
  arrondi au supérieur au palier de la grille de cette monnaie (domain_round_to : 500 FCFA)
```

Exemple : 6,49 € × 655,957 × 2 = 8 514,3 → **9 000 FCFA**.

Le palier vient de la grille de chaque monnaie (réglé dans « Grilles de
prix ») : un palier exprimé en FCFA ne s'applique jamais à une autre
monnaie. Le `round_to` de l'offre ne sert que si une grille n'en a pas.

Tout se règle dans le back-office, page **Offres VPS** (`/admin/vps-offers`) :
plan et prix du fournisseur, coefficient, caractéristiques, emplacements,
systèmes (nom chez le fournisseur), option snapshot, publication ; le prix de
vente calculé s'y affiche pour chaque monnaie. La page **VPS** (`/admin/vps`)
porte la réconciliation et le diagnostic (demande `compute.ping`).

Une offre n'est proposée que si elle est publique, a un plan fournisseur et un
prix. Emplacements autorisés : `gra`, `rbx`, `sbg` ; images : `ubuntu-24.04`,
`debian-12` (pas de Windows ; réinstallation par clé SSH seulement).

## Routes

| Méthode | Chemin | Droit |
|---|---|---|
| GET | `/organizations/:id/compute/offers` | `billing.accounts.get` |
| GET | `/admin/compute/offers` | `platform.billingCatalogs.get` |
| PUT | `/admin/compute/offers/:code` | `platform.billingCatalogs.update` |
| GET | `/organizations/:id/compute/vps` | `compute.vps.list` |
| GET | `/organizations/:id/compute/vps/:vpsID` | `compute.vps.get` |
| POST | `/organizations/:id/compute/vps` | `billing.payments.create` |
| POST | `/organizations/:id/compute/vps/:vpsID/start` | `compute.vps.start` |
| POST | `/organizations/:id/compute/vps/:vpsID/stop` | `compute.vps.stop` |
| POST | `/organizations/:id/compute/vps/:vpsID/reboot` | `compute.vps.reboot` |
| POST | `/organizations/:id/compute/vps/:vpsID/reinstall` | `compute.vps.reinstall` |
| PUT | `/organizations/:id/compute/vps/:vpsID/reverse` | `compute.vps.network` |
| POST | `/organizations/:id/compute/vps/:vpsID/terminate` | `compute.vps.delete` |
| POST | `/admin/compute/vps/:vpsID/spam/unblock` | `platform.compute.manage` |
| PUT | `/admin/compute/vps/:vpsID/smtp` | `platform.compute.manage` |
| POST | `/admin/compute/mailbox-check` | `platform.compute.manage` |
| GET, PUT | `/admin/compute/limits` | `platform.compute.get` / `.manage` |
| GET, PUT | `/admin/compute/organizations/:id/limits` | `platform.compute.get` / `.manage` |
| POST | `/admin/compute/vps/:vpsID/abuse/suspend` · `/abuse/resume` · `/terminate` · `/attach` | `platform.compute.manage` |
| GET | `/admin/compute/provider-mails` | `platform.compute.get` |
| POST | `/admin/compute/provider-mails/:mailID/handled` | `platform.compute.manage` |
| POST | `/organizations/:id/compute/vps/:vpsID/rescue` | `compute.vps.rescue` |
| POST | `/organizations/:id/compute/vps/:vpsID/rescue/exit` | `compute.vps.rescue` |
| POST | `/organizations/:id/compute/vps/:vpsID/rescue/password` | `compute.vps.rescue` |
| GET | `/organizations/:id/compute/vps/:vpsID/backups` | `compute.vps.get` |
| POST | `/organizations/:id/compute/vps/:vpsID/backups/restore` | `compute.vps.restore` |
| POST | `/organizations/:id/compute/vps/:vpsID/backups/unmount` | `compute.vps.restore` |
| POST, DELETE | `/organizations/:id/compute/vps/:vpsID/snapshot` | `compute.vps.snapshot` |
| POST | `/organizations/:id/compute/vps/:vpsID/snapshot/revert` | `compute.vps.restore` |
| POST | `/organizations/:id/compute/vps/:vpsID/snapshot/subscribe` | `billing.payments.create` |
| POST | `/organizations/:id/compute/vps/:vpsID/snapshot/unsubscribe` | `billing.payments.create` |
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
- `compute_requests` : outbox (pending → started → done / failed) ; closes gardées 90 jours.
- `compute_secrets` : secrets à usage unique (mot de passe initial), 15 min.
- `compute_provider_vms` : dernier relevé du fournisseur (état brut, plan, zone, adresses, écart).
- `compute_reconcile_runs` : passes de réconciliation (compteurs dont `unread`, erreur) ; gardées 30 jours.
- `compute_vps_backups` : relevé des sauvegardes de chaque VPS (lot 5).

## Configuration

| Variable | Où | Rôle |
|---|---|---|
| `KUBEMANAGER_COMPUTE_ENABLED` | API, worker, compute | routes, relais |
| `KUBEMANAGER_COMPUTE_TASK_QUEUE` | worker, compute | file Temporal (défaut `compute`) |
| `KUBEMANAGER_COMPUTE_ORDERING_ENABLED` | API | commandes réelles ouvertes |
| `KUBEMANAGER_COMPUTE_IMAP_*` | **compute seulement** | boîte technique (HOST, PORT 993, USERNAME, PASSWORD, MAILBOX) |
| `KUBEMANAGER_IAM_STS_KEY_ENCRYPTION_KEY` | API, compute | chiffrement des secrets (même clé sur les deux) |
| `KUBEMANAGER_COMPUTE_OVH_*` | **compute seulement** | clés limitées à `/vps/*`, `/order/*`, `/ip/*`, `/me`, `/services/*` (résiliation de l'option snapshot) |

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
# lecture réelle du compte (aucune commande) : liste des VPS, stock d'un plan
set -a; . ./.env; set +a; OVH_LIVE=1 OVH_PLAN=<plan de VPS-1> go test ./internal/modules/compute/repository/ovhvps -run TestLive -v
```

Back-office : page « VPS » (`/admin/vps` dans kweb).
