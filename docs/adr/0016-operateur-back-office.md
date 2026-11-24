# ADR 0016 — Opérateur : l'agent d'exploitation du back-office

Statut : proposé (2026-11-24) ; révisé le même jour (plan de gestion séparé). Rien n'est écrit.

## Contexte

L'équipe betorun fait tout le cycle depuis le back-office : santé des clusters, VPS, tickets des clients, comptes et facturation, page d'état. Chaque incident se règle à la main, d'un écran à l'autre. On veut un **agent IA d'exploitation**, l'Opérateur, qui couvre ce cycle :

- diagnostiquer Kubernetes sur tous les clusters, ainsi que la plateforme elle-même ;
- traiter les tickets des clients ;
- suivre les comptes, les soldes et les revenus ;
- réparer les pannes internes ;
- plus tard, piloter Proxmox.

L'objectif est d'avoir le meilleur harness possible : un agent fiable, traçable et sûr, pas un simple chat branché sur l'API.

**Ce qui existe déjà** et sert de socle :

- **Le copilot des clients** ([copilot.md](../modules/copilot.md)) :
  - une boucle modèle → outils → modèle (`copilot/agent`) ;
  - des outils générés depuis les routes (`ForCopilot`), avec un fichier de décisions par route et un test de couverture ;
  - une exécution avec un jeton délégué de la personne (`iam/sts/delegation.go`), en lecture seule par défaut ;
  - des écritures toujours par **carte d'action** validée par un humain ; les cartes sensibles (`Strong`) demandent une double authentification et le nom à retaper ;
  - une vérification des réponses (`grounding`) : un identifiant ou un montant sans preuve est réécrit ;
  - des budgets par personne.
- **La couche LLM** ([ADR 0008](0008-llm-provider-layer.md)) : fournisseur principal et de secours, disjoncteur, budget. Elle n'a qu'un adaptateur, compatible OpenAI ; l'adaptateur Anthropic natif y est prévu, pas écrit.
- **Le back-office** (`APP_MODE=platform`, routes `/admin`) : clusters (inscription, nœuds, contrôle, watcher), VPS, support (file, contexte, notes, actions groupées, temps réel), facturation (fiches, crédits, ajustements, postpayé, revenus), page d'état. La garde staff exige un motif, et chaque accès aux données d'un client entre dans son journal d'audit.
- **La santé des clusters** est relevée chaque minute par le worker (`cluster_health`). Les règles de diagnostic des conteneurs (`copilot/diagnosis` : OOM, port, image, crashloop…) sont pures et réutilisables.

**Ce qui manque :**

- aucune route `/admin` pour les pods, les events ou les logs d'un cluster ;
- aucune lecture de Prometheus ni de Loki par l'API ;
- aucun client Proxmox : les clusters sont des k3s sur VPS, et Proxmox est prévu (`ProviderProxmox`, ADR 0014) sans être installé ;
- des conversations du copilot dans Redis, sur 24 h, sans les résultats d'outils : c'est insuffisant pour une investigation à auditer.

## Décision

### 1. Un plan de gestion séparé : l'Opérateur ne tourne pas là où il répare

Un agent qui tourne sur la machine en panne ne la répare pas. L'Opérateur tourne donc **hors des serveurs qu'il surveille** : ni sur le serveur 1 (API, Postgres, annuaire, Temporal), ni sur les clusters des clients. Il vit sur un **serveur de gestion** dédié, petit, dans une autre zone (ou chez un autre hébergeur) que les serveurs 1 et 2. Ce serveur n'a aucun nom public et se joint par tunnel SSH, comme le back-office aujourd'hui.

**Deux chemins vers ce qu'il gère :**

- **Le chemin normal, par l'API du back-office** : support, commercial, page d'état, VPS des clients. Ces outils passent par les routes `/admin` du serveur 1, avec leurs gardes et leur audit. Si le serveur 1 est en panne, ces outils sont indisponibles et l'Opérateur le dit.
- **Le chemin de secours, direct et indépendant du serveur 1** : il sert à diagnostiquer et réparer l'infrastructure elle-même.
  - **API Kubernetes de chaque cluster**, avec une identité propre à l'Opérateur, distincte du compte `betorun-platform` de l'API. Ses droits sont ceux d'un rôle de dépannage : lecture de tout sauf les secrets, plus redémarrer un déploiement, supprimer un pod, revenir à la version précédente, mettre à l'échelle, isoler ou rouvrir un nœud. Jamais `exec`, jamais la lecture des secrets.
  - **Agent de nœud** sur chaque serveur (serveurs 1 et 2, nœuds des clusters) : un petit service en mTLS qui n'expose que des actions typées sur une liste fermée. On peut lire l'état et les journaux d'un service connu (conteneurs de la stack, k3s), redémarrer un de ces services, et lire disque, mémoire et charge. Pas de shell : l'agent de nœud refuse tout ce qui n'est pas sur sa liste.
  - **API du fournisseur des machines** (OVH aujourd'hui, Proxmox demain), avec une clé dédiée limitée au redémarrage et au mode secours des serveurs de la plateforme. C'est le dernier recours quand une machine ne répond plus du tout. Ce ne sont jamais les VPS des clients : eux passent par le chemin normal.
  - **Métriques et journaux** : Prometheus et Loki, lus directement.

**Le serveur de gestion garde son propre état** : dossiers d'investigation, cartes et journal des actions, dans sa propre base. Il fonctionne même si Postgres est tombé sur le serveur 1. Les dossiers sont recopiés vers l'API quand elle répond, pour être joints aux tickets et aux incidents.

**Connexion de l'équipe :**

- **En temps normal**, par l'IAM de la plateforme, comme le back-office. L'Opérateur obtient un jeton délégué de la personne (voir §3).
- **Si l'annuaire ou l'API ne répondent plus**, par un **accès de secours** propre au serveur de gestion : comptes locaux nominatifs de l'équipe, clé de sécurité ou TOTP obligatoire, et accès réservé au tunnel SSH. Les actions faites ainsi restent limitées au chemin de secours (infrastructure, jamais les données des clients) et entrent dans le journal du serveur de gestion. Ce journal est recopié dans l'audit de la plateforme au retour de l'API.

**Le serveur de gestion se surveille lui-même** : une sonde externe sur la page d'état prévient l'équipe s'il ne répond plus.

### 1 bis. Deux binaires à part, pas un mode de l'API

L'Opérateur n'est **pas** un `APP_MODE` de l'API. Le démarrage de l'API dépend du serveur 1 (Postgres, migrations, Redis, annuaire, Temporal) : un mode `operator` refuserait de démarrer justement pendant une panne. Séparer les binaires sépare aussi les secrets (clé du modèle, clé de secours du fournisseur, identités de dépannage) et les versions : on peut corriger l'un quand l'autre est cassé.

- **`cmd/operator`, en Go, dans le dépôt de l'API.** C'est le cerveau : un seul exemplaire, sur le serveur de gestion. Il parle au modèle par le SDK Anthropic officiel pour Go. Il réutilise le code commun (boucle d'agent, cartes, vérification des réponses, règles de diagnostic, clients Kubernetes). Un test interdit à `cmd/operator` d'importer le routeur, les modules métier ou la configuration de l'API : il ne parle à l'API que par HTTP.
- **`node-agent`, en Rust, dans son propre dépôt (`betorun/node-agent`).** C'est la main : un exemplaire par serveur (serveurs 1 et 2, nœuds des clusters). Il n'y a pas d'IA dedans. Il exécute une liste fermée d'ordres typés, reçus seulement de l'Opérateur en mTLS, et refuse tout le reste. Rust convient à ce binaire : il est petit, privilégié et posé sur chaque machine, ne partage aucun code avec l'API et doit consommer très peu.

**Le copilot reste un produit à part.** Il sert les clients, dans l'API publique, avec leurs droits. L'Opérateur sert l'équipe, hors de l'API. Ils ne partagent ni conversations, ni budgets, ni clés de modèle, seulement du code. La boucle d'agent, les cartes et la vérification des réponses sortent de `copilot/` vers un paquet neutre (`internal/agentkit`) que les deux utilisent. Les tests du copilot garantissent qu'il ne change pas de comportement.

### 1 ter. Réglages de l'Opérateur

Par rapport au copilot :

- **Limites plus larges** par tour : 30 appels au modèle, 80 appels d'outils, 10 minutes. Une investigation suit souvent plusieurs pistes.
- **Pas de limite de taille** sur une session : un résumé automatique compacte les anciens tours, et les preuves restent consultables.
- **Lancement** par un membre de l'équipe, sur un sujet : un ticket, un cluster, une organisation, un incident, ou une question libre.

### 2. Claude par l'API d'Anthropic

L'Opérateur ajoute l'**adaptateur Anthropic natif** prévu par l'ADR 0008 (`llm/anthropic`) : appels d'outils, réponses en flux et mise en cache du prompt. Le cache compte ici : le prompt système, les outils et les runbooks sont longs et identiques d'un tour à l'autre.

- **Modèle** : `claude-opus-5-5` pour les investigations, par config (`llm.operator.model`). Un modèle plus léger (`claude-haiku-5-5`) sert aux tâches simples : résumé d'un ticket, tri de la file.
- **Pile** : la même que le copilot (disjoncteur, secours, budget), avec un **budget d'équipe** journalier distinct de celui des clients.
- **Clé** : un compte Anthropic à part, dont la clé n'est que sur le serveur de gestion. Si le fournisseur du modèle ne répond plus, la console de l'Opérateur reste utilisable à la main : outils, cartes et runbooks, sans agent.

### 3. Identité et sécurité : l'Opérateur n'a aucun droit propre

- **Qui agit** : l'Opérateur agit avec un **jeton délégué du membre de l'équipe** qui l'utilise (acteur `betorun:operator`). Il ne peut jamais faire plus que cette personne. Un agent de support n'a pas accès aux actions d'infrastructure ; un administrateur, oui.
- **Données d'un client** : le motif de la session (par exemple « ticket 4f2… : site en panne ») sert de `X-Betorun-Justification` à chaque lecture. Chaque accès entre donc dans le journal d'audit du client, avec l'acteur `betorun:operator` et le nom de la personne. Une session sans motif ne lit aucune donnée de client.
- **Lectures** : libres, dans la limite des droits de la personne.
- **Écritures** : **toujours par carte**, validée par un humain (décision du 2026-11-24 : l'agent propose, l'humain valide). Une carte montre l'action, la cible, la raison, l'effet attendu et l'annulation possible.
- **Cartes sensibles** (`Strong`) : double authentification et nom à retaper. Elles couvrent la suppression, la résiliation d'un VPS, un geste commercial, une action sur l'accès au cluster et l'envoi d'un e-mail à un client.
- **Contenu non fiable** : un ticket, un log ou un message d'erreur sont des **données**, jamais des instructions. Une carte proposée juste après la lecture d'un contenu non fiable porte cette provenance (« proposée après lecture du ticket … »). Une instruction trouvée dans un ticket ne déclenche jamais d'action.
- **Interdits** : aucun outil de commande libre. Ni shell, ni `kubectl exec`, ni SQL brut. Chaque action est une route typée, avec sa garde et son audit.

### 4. Les outils : des routes `/admin`, déclarées une par une

Comme pour le copilot, une route devient un outil par `ForOperator(...)`. Un fichier de décisions tranche chaque route `/admin` : outil, carte, carte sensible ou hors Opérateur. Un test échoue si une route n'a pas de décision. L'Opérateur ne voit donc rien qui n'existe déjà dans l'API, avec ses gardes.

| Domaine | Lecture | Actions (cartes) |
|---|---|---|
| Kubernetes, tous clusters | nœuds, pods, events, logs, déploiements, santé (`cluster_health`), règles de diagnostic | redémarrer un déploiement, supprimer un pod, revenir à la version précédente, mettre à l'échelle, isoler ou rouvrir un nœud |
| Plateforme interne | santé des services (Postgres, Temporal, Centrifugo, stockage, annuaire), métriques (Prometheus), journaux (Loki), workflows en échec | relancer un workflow, relancer le watcher d'un cluster |
| VPS | état, demandes, boîte des notifications du fournisseur, abus | suspendre ou reprendre, débloquer, relancer la réconciliation |
| Support | file, ticket, contexte, historique du client | brouillon de réponse (envoyé seulement après validation), note interne, priorité, assignation, fermeture |
| Commercial | fiches, soldes, consommation, comptes en grâce, revenus, gros consommateurs, risque de départ | crédit, ajustement, plafond postpayé, relance de paiement ou de réactivation (rédigée, envoyée après validation) |
| Page d'état | incidents, sondes | ouvrir ou mettre à jour un incident |
| Proxmox | plus tard (§7) | plus tard |

Nouvelles routes `/admin` nécessaires, utiles aussi sans l'Opérateur :

- pods, events, logs et déploiements d'un cluster ;
- requêtes Prometheus et Loki en lecture, sur une liste de requêtes autorisées ;
- comptes à surveiller (grâce, épuisement proche) ;
- workflows Temporal en échec.

### 5. Investigations enregistrées, pas des conversations

Une **session** est un dossier d'investigation en Postgres :

- **sujet**, motif, personne, modèle ;
- **étapes** : chaque appel d'outil avec ses entrées, un condensé du résultat et sa durée ;
- **cartes** proposées, validées ou refusées, et par qui ;
- **conclusion**.

Le dossier peut être rouvert, partagé dans l'équipe, joint à un ticket (en note interne) ou à un incident. Il sert aussi de post-mortem. Les résultats complets des outils sont gardés 30 jours, puis seul le condensé reste.

### 6. Runbooks et mémoire de l'équipe

Les procédures connues sont des fichiers Markdown du dépôt (`docs/runbooks/`) : un cluster ne répond plus, le watcher est en retard, Temporal est bloqué, un certificat refusé, un VPS bloqué pour spam… L'Opérateur les cherche par un outil (`search_runbooks`) et cite celui qu'il suit.

En fin d'investigation, il peut **proposer** un nouveau runbook ou une correction, relue par un humain comme une PR. Rien ne s'apprend en silence.

### 7. Proxmox : une interface, pas encore d'adaptateur

Proxmox n'est pas installé. On fixe seulement l'interface `Hypervisor` dans le domaine des clusters :

- nœuds, VM et stockage en lecture ;
- démarrer, arrêter, migrer une VM et prendre un instantané, en actions.

Les outils de l'Opérateur seront prêts à s'y brancher. L'adaptateur Proxmox (API REST, jeton dédié en lecture seule d'abord) sera un lot à part, quand les hôtes existeront.

### 8. Évaluation : la qualité se mesure

Le meilleur harness est celui dont on mesure les erreurs. Un **banc de scénarios** rejoue des pannes avec de faux clusters, de faux journaux et de faux tickets :

- crashloop dû à une dépendance `localhost` ;
- OOM ;
- nœud plein ;
- certificat en échec ;
- watcher muet ;
- ticket d'un client dont le porte-monnaie est épuisé ;
- ticket contenant une instruction piégée.

Pour chaque scénario, on note : la cause trouvée, les cartes proposées (justes, inutiles, dangereuses), le nombre d'étapes et le coût. Le banc tourne à chaque changement du prompt, des outils ou du modèle. Un scénario raté bloque la fusion.

### 9. Déclenchement proactif (plus tard)

Quand un cluster passe `down` ou `degraded`, ou qu'une alerte Prometheus se déclenche, l'Opérateur peut ouvrir seul une **investigation en lecture seule**. Il la publie sur le fil de l'équipe (`support:staff` ou un fil `operator`), avec ses hypothèses et les cartes prêtes à valider. Rien ne s'exécute sans un humain.

## Lots

1. **Socle** :
   - serveur de gestion et binaire `cmd/operator`, avec sa propre base ;
   - adaptateur Anthropic ;
   - dossiers d'investigation ;
   - chemin normal en lecture : support, commercial, clusters par l'API ;
   - chemin de secours en lecture : API Kubernetes de chaque cluster avec l'identité de dépannage, Prometheus ;
   - accès de secours de l'équipe ;
   - page de l'Opérateur dans la console du back-office (session kweb).
2. **Agent de nœud** : lecture d'abord, puis redémarrage des services de la liste fermée, par carte.
3. **Actions** : cartes Kubernetes, support et commerciales ; brouillons de réponses et de relances.
4. **Plateforme interne** : Prometheus, Loki, Temporal, runbooks.
5. **Banc d'évaluation** : à mettre en place dès le lot 1 pour les scénarios en lecture, puis à étendre à chaque lot.
6. **Proactif** : investigations ouvertes sur alerte.
7. **Proxmox**, quand les hôtes seront installés.

## Conséquences

- **Un troisième serveur** : petit, mais à maintenir (mises à jour, sauvegarde de sa base, rotation de ses clés). C'est le prix de pouvoir réparer quand le reste est en panne.
- **Coût et dépendance** : un deuxième fournisseur de modèle, payant au jeton, réservé à l'équipe. Le budget d'équipe et le cache du prompt bornent le coût. Le fournisseur de secours reste celui du copilot.
- **Effet de bord utile** : chaque capacité de l'Opérateur est d'abord une route `/admin` typée et gardée. Le back-office gagne donc des écrans (pods, logs, comptes à surveiller) même sans agent.
- **Garanties** : l'Opérateur ne contourne aucune garde. Un accès aux données d'un client reste visible dans son journal d'audit, avec la mention de l'agent.
- **Charge de maintenance** : le banc de scénarios devient un artefact à maintenir comme les tests.
