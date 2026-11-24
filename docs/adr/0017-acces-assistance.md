# ADR 0017 — Accès d'assistance : l'équipe agit chez un client, borné et visible

Statut : accepté (2026-11-24). Prérequis du lot 3 b de l'Opérateur ([ADR 0016](0016-operateur-back-office.md)).

## Contexte

L'équipe betorun n'a **aucun droit** sur les ressources des clients. Ses rôles (`roles/platform.admin`, `roles/platform.support`) ne valent que sur `platform` et ne contiennent aucune permission de client. L'héritage ne descend pas de `platform` vers une organisation pour ces rôles. C'est voulu, et cela doit rester vrai par défaut.

Pourtant, pour dépanner, il faut parfois agir sur un conteneur, un site ou une base d'un client : redémarrer, revenir à la version précédente, arrêter une base qui sature. Aujourd'hui, l'Opérateur le fait seulement au niveau Kubernetes (`k8s.*`). Il contourne alors le modèle du client : ses politiques de refus ne s'appliquent pas, et l'action n'apparaît pas dans son journal sous le nom de la personne.

Faits établis dans le code :

- **Liaisons** : `iam_bindings` (`Binding{Resource, Role, Principal, Condition}`), avec une condition d'expiration (`request.time < timestamp(…)`). `AddBinding` n'écrit ni audit ni notification ; `PolicyService.Set` le fait, `ReconcilePlatformRoles` aussi, à la main.
- **Refus** : une politique de refus d'organisation ne s'applique **pas** à une décision venue de `platform` (`applyDenies`). Elle s'applique à une liaison posée sur l'organisation ou un projet.
- **Double authentification** : elle est exigée pour les permissions `Sensitive` (`checkMFA`). Un jeton délégué ne peut jamais en exercer.
- **Journal du client** : `audit_log`, lu par organisation (`audit.List`). Le journal recopié de l'Opérateur y entre déjà.
- **Destinataires** : `alerts.Store.OrgRecipients` donne les propriétaires d'une organisation.
- **Tickets** : un ticket porte `OrganizationID` (et parfois un projet ou une ressource).

## Décision

### 1. Un accès d'assistance est une liaison temporaire posée chez le client

Une **attribution d'assistance** (`assistance_grants`) ouvre, pour **une personne nommée de l'équipe**, une liaison :

- sur `organizations/<id>` ;
- avec le rôle `roles/support.assistant` ;
- pour le principal `user:<personne>` ;
- avec une condition d'expiration.

Comme elle est posée chez le client :

- ses **politiques de refus s'appliquent** : un client peut interdire toute assistance par une politique de refus, et elle l'emporte ;
- chaque appel passe par le **même contrôle** que pour n'importe quel membre : la route vérifie la permission, et rien n'est contourné ;
- chaque action entre dans **son journal**, au nom de la personne.

La table est la source de vérité. La liaison en découle : elle est posée à l'ouverture, retirée à la révocation ou à l'expiration. Un rapprochement périodique retire toute liaison `roles/support.assistant` sans attribution active, par exemple après une restauration ou une édition manuelle.

### 2. Un rôle étroit, jamais choisi par le client

`roles/support.assistant` est un rôle prédéfini, non attribuable dans l'éditeur des accès : seul le service d'assistance le pose. Il contient :

| Domaine | Permissions |
|---|---|
| Lecture | `resourcemanager.organizations.get`, `resourcemanager.projects.get`, `resourcemanager.projects.list`, `workloads.workloads.list/get`, `workloads.logs.read`, `workloads.metrics.read`, `workloads.builds.list/get`, `workloads.domains.list`, `sites.sites.list/get`, `sites.deployments.list`, `sites.domains.list`, `databases.databases.list/get`, `addons.instances.list/get`, `compute.vps.list/get` |
| Actions réversibles | `workloads.workloads.restart`, `workloads.workloads.rollback`, `workloads.workloads.start`, `workloads.workloads.stop`, `sites.deployments.activate` (retour à un déploiement), `databases.databases.stop` (arrêt et redémarrage), `compute.vps.start`, `compute.vps.stop`, `compute.vps.reboot` |

**Exclus** : toute création, modification ou suppression ; les variables et secrets (`workloads.variables.*`, `*.connect`) ; les fichiers du stockage ; l'IAM ; la facturation ; le journal d'audit ; toute permission `Sensitive`. La mise à l'échelle passe par `PATCH /workloads/:id` (`update`) : elle reste hors du rôle tant qu'une route dédiée n'existe pas.

Aucune permission du rôle n'est `Sensitive` : un jeton délégué en écriture (cartes de l'Opérateur) peut les exercer. Ce qui est sensible, c'est **l'ouverture** de l'accès (§3).

### 3. Deux manières d'ouvrir l'accès

**a. Le client accorde (voie normale).** Une personne de l'équipe **demande** l'accès depuis un ticket. Il faut `platform.assistance.request` (support et administrateurs), un motif et une durée souhaitée. La demande apparaît dans la console du client, sur le ticket et dans un bandeau de l'organisation. Les propriétaires sont prévenus par e-mail.

Un propriétaire, ou un administrateur de la sécurité, **accepte** (`resourcemanager.organizations.grantAssistance`, `Sensitive`). Il choisit la durée : 4 h par défaut, 24 h au plus. Il peut aussi refuser. Il peut enfin accorder de lui-même, depuis un ticket, sans demande préalable : l'accès va à l'agent assigné au ticket. Une demande sans réponse expire après 72 h.

**b. Bris de glace (incident).** Une personne de l'équipe ouvre l'accès sans accord quand le client est injoignable et que sa ressource menace la plateforme ou ses propres données : boucle de redémarrages qui sature un nœud, base qui remplit un disque partagé. Il faut :

- `platform.assistance.breakglass` (`Sensitive` : double authentification récente ; administrateurs seulement) ;
- un motif de 20 caractères au moins ;
- un ticket (le bris de glace part d'un ticket, comme la demande).

L'accès dure **1 h au plus**. Il n'est pas renouvelable sans nouveau bris de glace. Les propriétaires sont prévenus **immédiatement**, et le motif figure dans l'e-mail.

Dans les deux cas, une politique de refus du client qui vise la personne ou `allAuthenticatedUsers` sur ces permissions reste plus forte : l'accès est ouvert mais n'accorde rien.

### 4. Visible et révocable par le client

- **Journal** : `organization.assistance.requested`, `.granted`, `.denied`, `.breakglass`, `.revoked` et `.expired` entrent dans `audit_log` de l'organisation, avec la personne, le motif, le ticket et l'échéance. Les actions faites pendant l'accès y sont déjà, comme pour tout membre.
- **Console du client** : une page « Accès de l'équipe » (`/team-access`) liste les attributions (demandées, actives, passées). Une attribution active se révoque d'un clic, avec un effet immédiat : liaison retirée, cache invalidé. Révoquer et refuser une demande demandent `resourcemanager.organizations.revokeAssistance`. Cette permission n'est **pas** sensible : retirer un accès ne doit jamais être plus difficile que l'accorder.
- **Fin** : à l'échéance, la condition cesse d'accorder, même avant le passage du ramasseur. Le ramasseur retire ensuite la liaison et inscrit `expired`.
- Une personne de l'équipe voit ses propres accès dans le back-office (`GET /admin/assistance`, lecture du support) et peut les clore elle-même.

### 5. L'Opérateur (lot 3 b)

Les actions sur les ressources d'un client deviennent des **cartes de l'API** (`api.*`, ADR 0016 §3 bis) sur les routes du client. Elles suivent le même chemin que les cartes `/admin` : l'API fige la requête, la personne valide avec sa session, et la carte est jouée avec un jeton délégué en écriture. Leurs noms commencent par `customer_` ; lectures et cartes sont déclarées dans `operator_decisions.go`, et un test refuse une permission hors de `roles/support.assistant`.

Les outils `customer_*` n'entrent dans le catalogue de la personne que si elle a **au moins un accès actif**. La permission d'un outil client vaut chez le client, pas sur `platform` : l'API ne la teste donc pas sur `platform`. C'est la route du client qui décide, à chaque appel, avec le jeton de la personne.

Une carte sur une ressource client n'est **proposable** que si la lecture de sa cible passe avec le jeton de la personne, ce qui prouve l'accès sur l'organisation de la ressource. Sinon, l'API répond au modèle de demander l'accès au client, et l'agent peut proposer la carte `api.request_assistance`, qui crée la demande depuis le ticket.

**À la validation**, la personne qui valide relit la cible avec sa session : sans son propre accès, la validation est refusée et la carte reste en attente. La route de l'action vérifie encore la permission à l'exécution : un accès expiré ou révoqué entre-temps fait échouer la carte, et rien n'est joué.

Le contenu des bases (`…/contents`), les variables et les fichiers restent hors Opérateur.

Le bris de glace n'est **jamais** proposé par l'agent : c'est une décision humaine, prise dans le back-office.

## Conséquences

- Le principe « l'équipe n'a aucun droit chez un client » reste vrai par défaut. Toute exception est nommée, bornée dans le temps, visible du client et révocable.
- Les actions de l'équipe chez un client passent par le même contrôle d'accès, les mêmes refus et le même journal que celles des membres. Le niveau Kubernetes (`k8s.*`) reste pour la plateforme elle-même et pour les cas où l'API est en panne.
- Coût : une table, un service, deux pages (client et back-office), quatre permissions, un rôle réservé, un rapprochement périodique. Pas de nouveau mécanisme d'autorisation.
- Plus tard : des conditions plus fines (un projet, une ressource) quand les conditions IAM sauront exprimer plus que l'expiration. La liaison pourra alors être posée sur `projects/<id>` au lieu de l'organisation.

## Lots

1. **IAM** : permissions, rôle, table `assistance_grants`, service (demande, octroi, refus, bris de glace, révocation, rapprochement), journal et e-mails, routes `/organizations/:id/assistance` et `/admin/assistance`.
2. **Consoles** : page « Accès de l'équipe » côté client, demande et bris de glace côté back-office (kweb, betorun-f2).
3. **Opérateur** : cartes sur les routes du client, filtrées par l'accès actif, et carte `api.request_assistance`.

Lots 1 et 3 faits côté API (module : [assistance.md](../modules/assistance.md)) ; lot 2 confié à la console.
