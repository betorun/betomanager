# ADR 0019 — VPC : réseau privé du projet étendu à ses VPS

Statut : proposé (2026-11-26). Rien n'est codé.

## Contexte

Un projet a déjà un réseau privé ([module network](../modules/network.md)) :
ses conteneurs, services et bases se joignent par leur nom (`http://api`,
`cache:6379`, `db-main-rw:5432`), jamais ceux d'un autre projet. La doc du
module le pose déjà : décrire le réseau **sans rien du système qui
l'applique** (NetworkPolicy aujourd'hui, pare-feu d'un autre hébergeur
demain, Proxmox, VPS reliés au projet).

Les VPS d'un projet ([module compute](../modules/compute.md)) restent hors de
ce réseau : un VPS ne joint la base du projet que par Internet, ce qui
oblige à exposer la base publiquement. La landing met pourtant en avant
« VPS et VPC ». C'est le maillon qui manque pour qu'un client mette un
serveur à côté de ses conteneurs et de ses bases.

betorun aura deux sortes d'hébergement : des VPS achetés à un fournisseur
(aujourd'hui OVH) et ses propres machines sous Proxmox. Le VPC doit être le
même produit sur les deux.

### Ce que permet le fournisseur actuel (vérifié le 2026-11-26)

Schéma officiel de l'API (`/1.0/vps.json`, `/1.0/vrack.json`,
`/1.0/ip.json`, `/1.0/cloud.json`) et feuille de route publique :

| Besoin | VPS chez le fournisseur | Conséquence |
| --- | --- | --- |
| Réseau privé natif (vRack) | **Non.** `/vrack/{sn}` accepte serveurs dédiés, Public Cloud, Hosted Private Cloud, IP Load Balancing ; aucun VPS. Le fournisseur a écrit en octobre 2025 ne pas prévoir le vRack sur VPS. | Mise en œuvre « tunnel » (§ 4). |
| Script au premier démarrage | **Oui**, `postInstallScript` de `POST /vps/{sn}/rebuild` (bêta), exécuté une fois ; déjà porté par `ComputeProvider.Rebuild(…, script)`. | Le VPS rejoint le VPC dès sa création. |
| Pare-feu | **Oui**, pare-feu de bord (`/ip/{ip}/firewall/...`, production) : par IP, **sans état**, 20 règles au plus (séquences 0 à 19), IPv4, `permit`/`deny`, option TCP `established`. | Mise en œuvre du pare-feu (§ 6). |
| IP déplaçable | **Oui**, IP additionnelles (`POST /ip/{ip}/move`, production), /32 seulement, 16 au plus par VPS. | IP flottante possible plus tard (hors ADR). |
| Réseau privé, IP flottante, équilibreur, volumes | Seulement en Public Cloud (`/cloud/project/...`) : autre produit, autres prix. | Option écartée. |

## Décision

### 1. Un VPC = le réseau privé d'un projet, VPS compris

Pas de nouvel objet à créer : **chaque projet a un VPC**, celui qui existe
déjà pour ses conteneurs et ses bases. Rattacher un VPS du projet au VPC lui
donne les mêmes droits qu'un conteneur du projet : il joint tous les membres,
et les membres le joignent. Un VPS d'un autre projet ne voit rien.

Contraintes v1 :

- le VPS et le cluster du projet sont dans la **même région** ; sinon le
  rattachement est refusé (`VPC_REGION_MISMATCH`) ;
- un VPS appartient à **un seul** VPC, celui de son projet ;
- IPv4 seulement.

### 2. Indépendant de l'hébergeur

Le produit (modèle, API, console, CLI, codes d'erreur, journal) ne parle que
de VPC, de membres, d'adresses privées, de noms et de règles. **Rien ne
nomme l'outil ni le fournisseur** : ni WireGuard, ni Kubernetes, ni OVH, ni
Proxmox, ni « tunnel » (marque blanche, comme le module network).

La mise en œuvre passe par deux interfaces du domaine, comme
`ComputeProvider` pour les VPS ; chaque implémentation passe la même suite
de tests de contrat :

```go
// VPCBackend : relie les VPS d'un projet à son réseau privé.
type VPCBackend interface {
    Name() string                 // identifiant stable, jamais montré au client
    Capabilities() VPCCapabilities
    // Ensure : le réseau du projet existe et connaît ces membres (idempotent).
    Ensure(ctx context.Context, vpc VPC, members []VPCMember) error
    // Attach : prépare l'entrée d'un VPS ; rend ce que le VPS doit appliquer
    // (rien pour un réseau natif, une inscription pour un tunnel).
    Attach(ctx context.Context, vpc VPC, vps VPCMember) (*AttachPlan, error)
    Detach(ctx context.Context, vpc VPC, vpsID string) error
    // Status : état vu par le réseau (relié, dernier contact).
    Status(ctx context.Context, vpc VPC) ([]VPCMemberStatus, error)
    // Teardown : plus aucun VPS rattaché.
    Teardown(ctx context.Context, vpc VPC) error
}

// FirewallProvider : filtrage depuis Internet des adresses publiques d'un VPS.
type FirewallProvider interface {
    Name() string
    Capabilities() FirewallCapabilities // règles max, avec ou sans état, IPv6
    Apply(ctx context.Context, vps string, rules []FirewallRule) error // remplace tout
    Get(ctx context.Context, vps string) ([]FirewallRule, error)
}
```

- **Choix de l'implémentation** : par l'hébergement du VPS (fournisseur du
  VPS, colonne déjà connue du module compute) et le fournisseur du cluster
  (`clusters.provider` : `ovh` | `proxmox` | `other`). Stocké en interne sur
  le rattachement (`backend`), jamais rendu par l'API.
- **Capacités** : la console et l'API lisent `Capabilities()` (nombre de
  règles du client, IPv6, avec état) au lieu de chiffres en dur ; une limite
  propre à un hébergeur n'est jamais une règle du produit.
- **Plage d'adresses du VPC** : attribuée par la plateforme, indépendante de
  l'implémentation ; un VPC garde ses adresses s'il change d'implémentation.

| Hébergement | `VPCBackend` | `FirewallProvider` |
| --- | --- | --- |
| VPS d'un fournisseur sans réseau privé (OVH aujourd'hui) | `tunnel` (§ 4) | pare-feu de bord du fournisseur |
| Machines betorun sous Proxmox | `native` : réseau du SDN Proxmox (VXLAN/EVPN) relié au réseau du cluster, sans passerelle | pare-feu Proxmox (avec état) |
| Fournisseur qui offre un réseau privé | `native` propre à ce fournisseur | son pare-feu |

### 3. Adresses et noms (communs à toutes les implémentations)

- **Plage** : un /24 par VPC dans `10.250.0.0/16` (hors des plages des pods
  `10.42.0.0/16` et services `10.43.0.0/16` des clusters) ; chaque VPS une
  adresse fixe gardée tant qu'il est rattaché. Choix de la plage par le client :
  plus tard.
- **Noms** : suffixe `.internal` : `db-main-rw.internal` → la base du
  projet, `<nom-du-vps>.internal` → le VPS. Le VPS n'envoie au réseau privé
  que ce suffixe (domaine de routage DNS) ; le reste de sa résolution ne
  change pas.
- **Politique** : celle du projet (module network, `PolicyRules`) : un VPS
  rattaché est un membre comme un autre.

### 4. Implémentation `tunnel` (fournisseur sans réseau privé)

```
VPS du client ──WireGuard (UDP)──▶ passerelle du VPC ──▶ conteneurs, services, bases
 10.250.x.y/32                    pod dans l'espace       (NetworkPolicy du projet)
                                  du projet
```

- **Passerelle** : un conteneur `service` géré par betorun dans l'espace du
  projet (chart `standard-app`, non root, seccomp), créé au premier VPS
  rattaché (`Ensure`), supprimé au dernier (`Teardown`). Elle porte
  l'étiquette de membre du projet : les NetworkPolicy existantes la laissent
  joindre les membres ; elle ne relaie jamais vers un autre espace.
- **Entrée UDP** : un port par VPC, pris dans une plage réservée du nœud
  passerelle (`VPC_UDP_PORT_RANGE`), exposé par le LoadBalancer du cluster ;
  seules les IP publiques des VPS rattachés y sont admises.
- **DNS** : la passerelle répond pour `.internal`.
- **VPS ↔ VPS** : par la passerelle (étoile).
- **La clé privée ne quitte jamais le VPS** : le script de premier démarrage
  installe le client, génère la clé sur le VPS, puis envoie la clé publique
  à l'API avec un **jeton d'inscription** à usage unique, valable 30 min,
  lié à ce VPS. L'API renvoie la configuration (adresse, clé publique et
  point d'entrée de la passerelle). Le script ne contient aucune clé : il
  transite par le fournisseur.

### 5. Rattacher et détacher (produit, toutes implémentations)

- **À la création** : case « Rattacher au réseau privé du projet », cochée
  par défaut. Avec `tunnel`, le rattachement passe par le script de premier
  démarrage ; avec `native`, le VPS naît sur le réseau.
- **VPS existant** : bouton sur la fiche du VPS et `betorun vps vpc attach
  <vps>`. Avec `tunnel`, une commande à lancer en root sur le VPS est donnée
  (jeton de même nature) ; avec `native`, rien à faire sur le VPS.
- **Détachement** : effet immédiat côté plateforme (`Detach`), même si le VPS
  est injoignable.
- **Réinstallation, résiliation, suspension** : rattachement retiré dans la
  même transaction que l'effet métier (outbox du module compute), refait à la
  réinstallation si la case est cochée.
- **État** : la console montre l'état vu par le réseau (`Status` : relié,
  dernier contact), car le client, root sur son VPS, peut couper le lien.

Permissions : `compute.vps.network` (existe, DNS inverse) couvre rattacher
et détacher ; lecture par le module network. Journal d'organisation :
rattachement, détachement, refus.

### 6. Pare-feu des VPS

Le produit : des règles ordonnées (protocole, port ou plage, source IP ou
bloc, autoriser ou refuser), appliquées à l'entrée depuis Internet ; le
trafic du VPC suit la politique du projet, pas ce pare-feu.

- **Règles réservées à betorun**, ajoutées par l'implémentation et jamais
  modifiables : entrée du réseau privé, port 25 sortant fermé (règle
  actuelle) et, si le pare-feu est sans état, retour TCP `established`.
- **Règles du client** : autant que `FirewallCapabilities` en laisse (16 sur
  le pare-feu de bord actuel, 20 règles moins 4 réservées ; davantage avec
  Proxmox).
- Modèle par défaut proposé : « tout refuser sauf SSH ».

## Découpage

| Lot | Contenu |
| --- | --- |
| 1 | Modèle et API : interfaces `VPCBackend` et `FirewallProvider` avec suite de contrat, plage du VPC, membres VPS dans `GET /projects/:id/network`, rattacher, détacher, jetons d'inscription (migration, permissions, journal). Implémentation de test seulement. |
| 2 | Implémentation `tunnel` : passerelle (image figée, chart, port UDP), script de premier démarrage, commande pour un VPS existant, détachement, réinstallation, résiliation. |
| 3 | Noms `.internal`, VPS ↔ VPS, page Réseau de la console (VPS dans le schéma, état du lien), CLI. |
| 4 | Pare-feu : modèle, `FirewallProvider` du fournisseur actuel, page et CLI. |
| Proxmox | Implémentations `native` et pare-feu Proxmox, avec l'arrivée des machines betorun ; aucun changement de produit. |
| Après | IP flottante, équilibreur de charge pour VPS, volumes : chacun son ADR, sur les mêmes principes. |

## Options écartées

- **Passer les VPS sur le Public Cloud du fournisseur** (réseau privé, IP
  flottante, équilibreur, volumes natifs) : prix d'achat et grille différents,
  module compute à réécrire, et dépendance accrue à un fournisseur que
  Proxmox doit remplacer. Avec ce découpage, ce serait au plus une
  implémentation `native` de plus.
- **Attendre le vRack sur VPS** : rien d'annoncé.
- **Rattacher le VPS au réseau du cluster** (agent réseau du cluster sur la
  machine du client) : il faudrait ouvrir au client le plan de contrôle du
  cluster et lui confier un composant de la plateforme.
- **Maillage tiers** (Tailscale, Headscale, Netbird) : un service de plus à
  opérer et à sécuriser, pour ce que WireGuard fait seul.
- **Configuration complète dans le script de création** : la clé privée
  passerait par le fournisseur.

## Conséquences

- Avec `tunnel`, le trafic VPC traverse la passerelle et la bande passante du
  nœud passerelle : à mesurer (compteur par projet) avant d'en fixer le prix.
  **Prix et facturation : décision du propriétaire**, rien n'est proposé ici.
- Avec `tunnel`, la passerelle est un point unique par projet : redémarrée par
  le cluster, sans état (clés publiques et adresses en base, rechargées au
  démarrage). Haute disponibilité : plus tard, ou native avec Proxmox.
- `postInstallScript` est en bêta chez le fournisseur : le rattachement par
  commande reste toujours possible.
- Changer d'implémentation (VPS migré vers Proxmox) garde le VPC, ses
  adresses, ses noms et ses règles ; seul le rattachement est refait.
- Côté console : schéma du réseau avec les VPS, rattachement à la création et
  sur la fiche du VPS, état du lien, pare-feu, limites lues dans les
  capacités. À concevoir avec la DA de la console, sans jamais nommer l'outil.
