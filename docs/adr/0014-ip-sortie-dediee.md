# ADR 0014 — IP de sortie dédiée par projet

Statut : accepté (2026-11-23).

## Contexte

Des clients doivent se faire autoriser chez un partenaire (banque, API de paiement, base externe) par l'adresse IP d'où partent leurs conteneurs. Aujourd'hui, le trafic sortant d'un pod est traduit vers l'adresse du nœud qui le porte : elle est partagée par tous les clients du cluster et change avec le nœud. Une IP partagée ne protège rien (un partenaire qui l'autorise autorise tout le cluster) : il faut une IP **propre au projet**.

Les clusters à venir (Proxmox) et ceux d'aujourd'hui (k3s sur VPS OVH) doivent offrir la même chose, sans que la console ou l'API ne parlent de l'outil.

## Décision

1. **Cilium est le réseau standard des clusters** (k3s installé sans flannel ni kube-proxy de k3s, `--flannel-backend=none --disable-network-policy`). Sa passerelle de sortie (`CiliumEgressGatewayPolicy`) fait sortir les pods d'un projet par une IP donnée, posée sur un **nœud passerelle**, quel que soit le nœud du pod. Les NetworkPolicy Kubernetes existantes restent valables (Cilium les applique).
2. **Modèle neutre.** Une *adresse de sortie* (`egress_addresses`) appartient à un cluster et à un nœud passerelle ; elle est libre ou attribuée à un projet. Le cluster déclare sa passerelle (`clusters.egress_gateway` : vide = aucune, `cilium`). Le service parle à une interface `Gateway` (poser / retirer la règle d'un projet) ; l'implémentation Cilium vit dans `repository`, une autre (pare-feu Proxmox, agent maison) pourra s'y brancher sans toucher au reste.
3. **Réserve tenue par l'équipe.** L'équipe déclare au back-office les IP d'un cluster (achetées chez l'hébergeur et posées sur l'interface du nœud passerelle). Le client demande une IP depuis la page Réseau ; la plateforme en prend une libre sur le cluster du projet et pose la règle. Rendre l'IP retire la règle et la remet dans la réserve.
4. **Facturation** : compteur horaire `egress_ip` (plafonné au mois), prix mensuel dans la grille (`egress_ip_monthly`), jamais dans le code.
5. **Ce que voit le client** : sur la page Réseau, l'IP par laquelle sortent ses conteneurs (dédiée ou non disponible), le bouton pour en demander une ou la rendre, et son prix tiré de la grille.

## Découpage

| Lot | Contenu |
|---|---|
| 1 | Réserve par cluster (back-office), demande / restitution par le client, règle Cilium posée et retirée, compteur et prix, affichage sur la page Réseau. Les IP sont posées à la main sur le nœud passerelle. |
| 2 | Agent de nœud (posé par « Préparer le cluster ») qui monte les IP de la réserve sur l'interface du nœud passerelle ; achat et rattachement des IP additionnelles OVH depuis le back-office. |
| 3 | Fournisseur d'IP Proxmox (bloc routé vers la VM passerelle), passerelle de secours (deux nœuds), réconciliation périodique des règles. |
| Exploitation | Terrain réinstallé avec Cilium (préprod, coupure de quelques minutes) : `deploy/README.txt`. |

## Options écartées

- **Agent iptables maison sur flannel** : simple sur un nœud, mais plusieurs nœuds exigent de router tout le trafic vers la passerelle (tunnel, routage par source) : c'est réécrire une passerelle de sortie.
- **Proxy de sortie (HTTPS_PROXY)** : seul le trafic HTTP(S) qui respecte le proxy en profite ; ni les bases externes ni le courriel. Insuffisant pour le besoin « banque ».
- **IP partagée du cluster** : ne protège rien chez le partenaire.

## Conséquences

- Le compte plateforme reçoit les droits sur `cilium.io/ciliumegressgatewaypolicies` (objets de cluster) : `deploy/k8s/rbac.yaml` et `PlatformRules` (test de parité).
- Un cluster sans passerelle déclarée n'offre pas d'IP dédiée ; le placement d'un projet n'en tient pas compte (lot 3 si besoin).
- Une règle de sortie n'est appliquée que si l'IP est montée sur le nœud passerelle : sinon le trafic du projet est perdu. Lot 1 : l'équipe déclare une IP **après** l'avoir montée ; lot 2 : l'agent le garantit.
