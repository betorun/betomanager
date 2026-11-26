# ADR 0020 — Application mobile du back-office

Statut : proposé (2026-11-26). Maquettes validées le même jour (32 écrans). Rien n'est écrit.

## Contexte

L'équipe gère la plateforme depuis le back-office web (`APP_MODE=platform`, [console plateforme](../architecture/platform-console.md)). Sans ordinateur, elle ne peut rien faire : ni répondre à un ticket urgent, ni bloquer un VPS qui envoie du spam, ni valider une carte de l'Opérateur ([ADR 0016](0016-operateur-back-office.md)). On veut une **application mobile du back-office** pour suivre la plateforme et faire les gestes urgents depuis le téléphone.

**Ce qui existe déjà** et sert de socle :

- **La connexion de la CLI** (`identity/service/cliauth.go`) : PKCE (RFC 8252) avec validation dans la console, qui exige un second facteur récent quand la personne en a un. Le retour se fait sur `127.0.0.1`. Il existe aussi un flux par code d'appareil (RFC 8628).
- **Les facteurs** (`internal/auth`) : TOTP, codes de secours et **passkeys** (WebAuthn, `auth/passkeys.go`).
- **La double authentification** (`middleware/security/mfa.go`) : une action sensible exige un second facteur de moins de `StepUpMaxAge`, 10 minutes par défaut. Le refus suit la RFC 9470 (`STEP_UP_REQUIRED`).
- **Les routes `/admin`** couvrent déjà presque tout le périmètre :
  - le support : file, ticket, contexte, notes, messages, assignation et **macros** (les réponses types) ;
  - les VPS : SMTP, suspension et reprise pour abus, boîte du fournisseur ;
  - les clusters : fiche, nœuds, relevé, état ;
  - la facturation : fiches, crédits, revenus ;
  - la page d'état : incidents, mises à jour, résolution ;
  - les **cartes de l'Opérateur**, validées auprès de l'API (`/admin/operator/cards/:cardID/approve|reject`).
- **L'Opérateur** a sa console web, avec ses dossiers, ses cartes, son journal et la santé de l'agent. Il tourne sur un serveur de gestion séparé et n'est pas déployé en V1.

**Ce qui manque :** les notifications push, l'enregistrement des appareils, l'astreinte, une recherche transverse des clients et la détection des clés API à l'activité suspecte.

## Décision

### 1. Périmètre : suivi, gestes urgents et Opérateur

L'app couvre ce que les maquettes montrent :

| Domaine | Lecture | Gestes |
|---|---|---|
| Accueil | à traiter, état, activité, résumé du matin | — |
| Support | file, ticket, contexte | répondre, note interne, réponses types (macros), brouillon de l'Opérateur, résolu |
| Abus | e-mails du fournisseur | marquer traité |
| Infra — VPS | parc, écarts, fiche | bloquer le SMTP, suspendre pour abus, relire l'état |
| Infra — clusters | santé, nœuds, demandé sur allouable | relever, changer l'état |
| Facturation | comptes à solde bas, compte, revenus du jour | créditer, prévenir le client |
| Plateforme | services, clusters, incidents | publier un incident ou le mettre à jour |
| Opérateur | dossiers, conversation, cartes, journal, santé | ouvrir un dossier, donner un ordre (écrit ou dicté), valider ou refuser une carte, conclure |
| Moi | astreinte, appareils, sessions | passer la garde, révocation d'urgence |

Le reste reste sur le web : déclarer un cluster, gérer les clés des watchers, les grilles et plans, les quotas, les régions, les textes légaux, rattacher ou réinstaller un VPS.

**Règle de conception :** l'app ne fait rien que le web ne fasse pas. Chaque écran appelle des routes `/admin` existantes, avec leurs gardes et leur audit. Il n'y a ni route spéciale « mobile » pour agir, ni droit en plus.

### 2. Une application Flutter, distribuée en interne

- **Dépôt** : `betorun/backofficeapp`, commits signés comme les autres dépôts de l'organisation.
- **Pile**, reprise de `syk_app` : `riverpod`, `go_router`, `dio`, `flutter_secure_storage`. S'y ajoutent `local_auth` pour l'empreinte, `firebase_messaging` pour le push, une bibliothèque de passkeys natives et la reconnaissance vocale du système.
- **Langues** : français et anglais (fichiers ARB), avec les mêmes textes que la console quand l'écran est le même.
- **Style** : celui du back-office (IBM Plex, bordeaux `#7A1F3D`, liseré ambre qui distingue le back-office). Montants en XAF et XOF, jamais additionnés entre eux.
- **Distribution** : hors des boutiques publiques. Test interne de Google Play et TestFlight (ou distribution d'entreprise), réservés aux comptes de l'équipe. Une app de back-office n'a rien à faire dans une boutique publique.

### 3. Réseau : par le VPN de l'équipe

Le back-office et le serveur de l'Opérateur ne sont pas exposés sur Internet. Le téléphone les joint par un **profil WireGuard d'équipe**. Ce profil est nominatif et révocable, et il est réglé en tunnel partiel : seuls les domaines du back-office et de l'Opérateur passent par le VPN.

- L'app vérifie que le back-office répond. Sinon elle l'affiche (« VPN coupé ») et ne montre que ce qu'elle a en cache, marqué comme ancien.
- Les **push** n'ont pas besoin du VPN : ils partent des serveurs vers Google et Apple. Pour ouvrir le détail, il faut le VPN.
- Aucune surface publique nouvelle.

### 4. Connexion : comme la CLI, avec un lien d'app vérifié

- **Flux** : PKCE comme la CLI. Le retour se fait sur un **lien d'app vérifié** (App Links sur Android, Universal Links sur iOS) du domaine de la console plateforme, et non sur `127.0.0.1`. `cliauth` accepte un second client, `backoffice-mobile`, dont l'adresse de retour est fixée exactement par la configuration. Le retour sur `127.0.0.1` reste réservé à la CLI.
- **Validation** dans la console plateforme, ouverte dans l'onglet de navigateur du système (`ASWebAuthenticationSession`, Custom Tabs). Ce n'est pas une vue web intégrée, et il n'y a pas de formulaire de mot de passe dans l'app. Le second facteur est **obligatoire** pour ouvrir une session mobile, même si la personne n'en exige pas ailleurs.
- **Session** : la session est de type « mobile ». Elle apparaît dans la liste des sessions avec le nom de l'appareil, et on peut la couper depuis le web. Le jeton de renouvellement est gardé dans le Keystore ou le Keychain, avec un accès qui exige l'empreinte. Le jeton d'accès ne vit qu'en mémoire.
- **Déverrouillage** : l'empreinte est demandée à l'ouverture et après 5 minutes en arrière-plan. Après 3 échecs, la session locale est effacée et il faut se reconnecter par la console.
- **Écrans** : le contenu est masqué dans le sélecteur d'apps. Les captures sont bloquées sur les écrans qui portent des données client ou une carte.

### 5. Gestes sensibles : la passkey du téléphone

L'empreinte qui déverrouille l'app est locale : **elle ne prouve rien à l'API**. Pour un geste sensible, le second facteur est une **passkey** créée sur le téléphone. L'app utilise les API natives (Credential Manager, AuthenticationServices). La passkey est rattachée au domaine de la console (`assetlinks.json` et `apple-app-site-association`), et l'empreinte est le contrôle de présence de la passkey. L'API la vérifie comme une passkey du web (`auth/passkeys.go`). Le code TOTP saisi reste possible en secours.

Les règles ne changent pas :

- second facteur de moins de `StepUpMaxAge` (10 minutes par défaut) ;
- motif staff obligatoire ;
- nom à retaper pour une carte `Strong`.

L'app relance la vérification sur `STEP_UP_REQUIRED`, comme la CLI.

### 6. Notifications push

- **Nouveau module `devices`** (API, mode `platform`) : enregistrer, lister et révoquer un appareil (jeton FCM ou APNs, plateforme, libellé, personne). L'envoi est fait par le **worker**, puisque le mode `platform` n'a pas de boucle de fond. On passe par Firebase Cloud Messaging, qui relaie vers APNs.
- **Signaux** :
  - ticket urgent ou ticket assigné ;
  - abus signalé ;
  - cluster `down` ou `degraded` ;
  - incident ;
  - solde bas ;
  - carte de l'Opérateur à valider ;
  - dossier conclu ;
  - Opérateur injoignable.
  
  Chaque personne choisit ses catégories.
- **Ce qui part chez Google et Apple** : la catégorie, un texte court et l'identifiant de l'objet. Ce texte ne contient **jamais** d'e-mail, de montant, de nom de client ni de contenu de ticket. Les noms internes (cluster, VPS) sont permis. L'app charge le détail après le déverrouillage.
- **Actions sur la notification** : « Examiner » ouvre l'app. « Refuser » une carte demande le déverrouillage du téléphone. **« Valider » n'existe jamais sur l'écran verrouillé.**
- **Urgence** : canal à haute importance sur Android ; niveau « sensible au temps » sur iOS. Les alertes critiques d'iOS demandent l'accord d'Apple : ce sera plus tard, si besoin.

### 7. L'Opérateur entièrement pilotable depuis l'app

- **Même console, autre écran** : l'app parle au serveur de l'Opérateur par le VPN, avec les mêmes routes que sa console web :
  - dossiers ;
  - conversation en flux (SSE) ;
  - outils utilisés ;
  - cartes et journal.
  
  Le motif d'un dossier ouvre ou non les données du client, comme sur le web (ADR 0016 §3).
- **Ordres dictés** : la reconnaissance vocale du système tourne sur l'appareil quand il le permet. Le texte reconnu s'affiche et la personne l'envoie elle-même. Aucun ordre ne part sans avoir été relu.
- **Cartes** : une carte de l'API est validée **auprès de l'API**, avec la session mobile et la passkey si la carte est forte (ADR 0016 §3 bis). Pour les autres fournisseurs (agent de nœud, API Kubernetes), la validation passe par l'Opérateur, qui exige la même preuve fraîche que la console. Le moteur revérifie le risque avant d'exécuter.
- **Signaux sans le serveur 1** : l'Opérateur envoie lui-même ses push (cartes, dossiers conclus, pannes vues par le chemin de secours). Il utilise un compte de service Firebase à lui, limité à l'envoi. Ses notifications arrivent donc même si l'API est en panne. À l'inverse, le worker de l'API envoie « Opérateur injoignable » quand la sonde externe du serveur de gestion échoue. Chaque côté prévient pour l'autre.
- **Appareils** : l'app s'enregistre auprès de l'API et auprès de l'Opérateur. Révoquer l'appareil sur l'un le révoque sur l'autre dès que les deux se parlent.

### 8. Secours : diagnostic et alerte seulement

L'accès de secours de l'Opérateur reste celui de l'ADR 0016 : tunnel SSH avec une clé physique, depuis un ordinateur. Le téléphone **ne répare pas** :

- il montre ce qui répond (API, base, serveur de gestion, agent, file des tâches), d'après les sondes ;
- il alerte l'équipe et ouvre un incident.

L'écran « Session de secours » de la maquette perd ses trois gestes de réparation.

### 9. Fonctions qui demandent du nouveau côté serveur

| Fonction | Ce qu'il faut |
|---|---|
| Fiche client 360° | une route `/admin` de recherche par organisation, e-mail, IP ou domaine, qui agrège les données existantes ; la consultation passe par le motif et entre dans le journal de l'organisation |
| Astreinte | planning des gardes, relance après 5 minutes, escalade après 10 minutes (worker) |
| Révocation d'urgence | signal « clé API suspecte » (pays, volume) et révocation `/admin` d'une clé client ; mes sessions : routes `/identity` existantes |
| Résumé du matin | investigation programmée de l'Opérateur, en lecture seule, avec le modèle léger (ADR 0016 §9) |
| Widget | uniquement des nombres et l'état, mis à jour par les push ; aucun nom ni montant |
| Revenus du jour | `GET /admin/billing/revenue` existe ; il faut le détail par moyen de paiement et la liste des échecs |

Chaque nouvelle route `/admin` reçoit sa décision Opérateur dans le même lot (`operator_decisions.go`, `TestOperatorCoverage`).

### 10. Téléphone perdu ou compromis

- On révoque l'appareil depuis le web, ce qui coupe la session, le jeton push et la passkey. On coupe aussi son profil WireGuard.
- Sur un appareil rooté ou jailbreaké, l'app avertit et **refuse les gestes sensibles**.
- L'app ne garde en cache que le dernier état affiché. Ce cache est chiffré et effacé à la déconnexion.

## Lots

1. **Socle** :
   - squelette de l'app et VPN ;
   - connexion PKCE avec lien vérifié (client `backoffice-mobile` dans `cliauth`) ;
   - déverrouillage et passkey de l'appareil ;
   - Accueil, Support, Abus, Infra (VPS et clusters), Facturation, Plateforme, publication d'incident ;
   - lectures et gestes sur les routes existantes.
2. **Push** : module `devices`, envoi par le worker, préférences, notifications actionnables.
3. **Opérateur sur mobile** : dossiers, conversation, ordres dictés, cartes, journal, push de l'Opérateur. Ce lot suit le déploiement de l'Opérateur.
4. **Ajouts** : fiche client 360°, revenus détaillés, révocation d'urgence, widget, diagnostic de secours.
5. **Astreinte** : planning, relance, escalade.
6. **Résumé du matin**, avec le déclenchement proactif de l'Opérateur.

## Conséquences

- **Un deuxième client à maintenir.** Chaque écran du back-office web qui change peut demander le même changement sur mobile. Le périmètre fermé du §1 limite cette charge.
- **Des dépendances nouvelles** : Firebase (push) et les comptes développeur Google et Apple. Les textes envoyés sont pensés pour ne rien révéler des clients.
- **Le VPN devient obligatoire** pour l'équipe sur mobile. C'est voulu : aucune surface publique de plus.
- **Garanties inchangées** : mêmes droits, même double authentification, même motif, même audit que le web. L'Opérateur reste sans droit propre, et toute écriture passe par une carte validée par un humain.
- **Effet utile hors mobile** : le module `devices` et l'astreinte servent aussi aux alertes de l'équipe sur le web.
