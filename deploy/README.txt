DÉPLOYER BETORUN EN PRODUCTION
==============================

Serveur 1 : API, worker, console, Postgres, Redis, Temporal, Keycloak, Centrifugo,
            registre, stockage objet, Caddy (docker compose).
Serveur 2 : k3s avec son Traefik intégré (conteneurs des clients, builds, et
            static-edge qui sert les sites statiques : section 7 ter).

Dans ce guide, <domaine> est votre domaine, <IP1> et <IP2> les IP publiques.


0. CE QUI A ÉTÉ VÉRIFIÉ, ET CE QUI NE L'A JAMAIS ÉTÉ
----------------------------------------------------

Vérifié sur une seule machine de développement, le 2026-11-02 :
  - construction et contenu des images API/worker, static-edge, console ;
  - refus de démarrer d'une configuration incomplète ;
  - docker-compose.prod.yaml service par service : Postgres et ses rôles, Redis,
    Temporal (schéma, serveur, espace de noms), Keycloak en mode start et son
    bootstrap, Centrifugo (origines), registre (realm), SeaweedFS, API, worker,
    static-edge, migrations par l'API seule, passage en panne de Redis ;
  - routes du Caddyfile en HTTP sur le réseau interne : blocage des sondes,
    compression, adresse réelle du client ;
  - RBAC, chart, redirection HTTPS, NetworkPolicy sur un k3s 1.36.5 jetable,
    avec un solveur HTTP-01 SIMULÉ (mêmes labels, même forme d'Ingress).

JAMAIS TESTÉ EN VRAI, à surveiller au premier déploiement :
  - HTTPS réel, certificats Let's Encrypt ;
  - static-edge dans le cluster (2026-11-19) : lecture des sites par l'API avec
    la clé du cluster, fichiers par S3_PUBLIC_HOST, certificat joker DNS-01,
    Ingress des domaines clients créés par le worker. Code testé (tests
    unitaires, faux cluster), manifestes validés à blanc ; aucun essai réel ;
  - cert-manager réel et son solveur HTTP-01 ;
  - les deux serveurs ensemble : API vers k3s, build, push et pull du registre
    par son nom public, de bout en bout ;
  - tous les services démarrés en même temps, console et Caddy compris : la consommation mémoire réelle ;
  - la console à travers Caddy, avec son extra_hosts ;
  - l'envoi d'emails par un vrai SMTP ;
  - le job deploy de la CI (jamais déclenché) ;
  - la restauration d'une sauvegarde (à répéter avant d'en avoir besoin).

Ajouté depuis le 2026-11-03, vérifié sur un cluster k3s 1.36.5 distant (compte
betorun-platform de rbac.yaml) mais avec l'API sur un poste de développement :
bases gérées (CloudNativePG 1.30.1, plugin Barman Cloud v0.15.1), import d'un dump,
contenu, remise à zéro et restauration de sa copie, builds depuis GitHub, double
authentification (acr=gold). JAMAIS TESTÉ avec l'API derrière Caddy : les Jobs des
bases téléchargent et envoient par https://api.<domaine> ; webhook GitHub réel ;
sauvegardes continues des bases vers un S3 externe.


1. PRÉREQUIS
------------

Serveur 1 : Linux, Docker Engine et le plugin compose, 6 Go de RAM conseillés
  (somme des plafonds : environ 5,7 Go ; consommation réelle à mesurer), 8 Go avec
  le profil observability. Prévoir du disque pour les images et les sites.
Serveur 2 : Linux, 4 Go de RAM au moins (un build peut en prendre 2).
Machine de construction de la console : Docker. Pas le serveur 1 : next build
  demande environ 2 Go.
À préparer :
  - un compte SMTP (hôte, port, utilisateur, mot de passe, adresse d'expédition) ;
  - les emails du staff (PLATFORM_ADMINS) ;
  - un dépôt d'images (Docker Hub) ; la CI publie thekrauss/kubemanager et
    thekrauss/static-edge sous le tag du commit (section 10) ;
  - une application GitHub de PRODUCTION, distincte de celle du développement
    (section 5 bis) ;
  - facultatif : un bucket S3 d'un autre fournisseur pour les sauvegardes
    continues des bases des clients (DATABASES_BACKUP_* dans .env.prod).


2. DNS
------

  Nom                       Type    Valeur   Rôle
  app.<domaine>             A       <IP1>    console
  api.<domaine>             A       <IP1>    API
  registry.<domaine>        A       <IP1>    registre d'images
  realtime.<domaine>        A       <IP1>    Centrifugo (wss)
  s3.<domaine>              A       <IP1>    stockage des sites, pour static-edge
  edge.<domaine>            A       <IP2>    cible CNAME des domaines de sites
  *.sites.<domaine>         A       <IP2>    sites statiques (static-edge, cluster)

Keycloak n'a aucun nom public, et n'en aura pas.

Conteneurs des clients : <conteneur>-<projet>.<IP2>.sslip.io, sans enregistrement
à créer. Leurs domaines personnalisés pointent vers <IP2>, comme ceux des sites
(CNAME vers edge.<domaine>).

PAS D'ENREGISTREMENT AAAA vers le serveur 1 tant que le réseau Docker n'a pas
IPv6 : un client en IPv6 passerait par le proxy de Docker, et Caddy verrait
l'adresse de la passerelle au lieu de la sienne. Tous ces clients partageraient
alors les mêmes limites.

Créer le DNS AVANT de démarrer Caddy : il demande ses certificats au démarrage,
et Let's Encrypt limite les échecs répétés.


3. PARE-FEU
-----------

Serveur 1, ouvert à tous : 80/tcp, 443/tcp, 443/udp. SSH pour l'administration.
  Docker contourne ufw : un port publié par compose est ouvert quoi que dise ufw.
  C'est pourquoi le compose de production ne publie que ceux de Caddy.
  Sortant : 6443 vers <IP2>, SMTP, 443 (Let's Encrypt, GitHub).

Serveur 2 :
  80/tcp, 443/tcp   ouverts à tous (Traefik)
  6443/tcp          ouvert UNIQUEMENT à <IP1>
  SSH               pour l'administration ; rien d'autre.

    sudo ufw default deny incoming
    sudo ufw allow OpenSSH
    sudo ufw allow 80/tcp
    sudo ufw allow 443/tcp
    sudo ufw allow from <IP1> to any port 6443 proto tcp
    sudo ufw enable

Ce que le serveur 2 doit joindre sur le serveur 1 : uniquement 443, par les noms
publics api.<domaine> (archives des builds, jetons du registre et du STS, table des
sites pour static-edge), registry.<domaine> (push et pull) et s3.<domaine>
(fichiers des sites, lectures seules, réservé à <IP2> par Caddy). Aucun autre port
n'est à ouvrir.

Si les deux serveurs sont sur un même réseau privé et que ces noms y résolvent en
adresses privées : les NetworkPolicy des builds bloquent les plages privées, il
faudrait alors BUILDS_EGRESS_ALLOW_CIDRS dans .env.prod. Le plus simple est de garder
les IP publiques.


4. SERVEUR 2 : K3S
------------------

    curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='v1.36.5+k3s1' \
      INSTALL_K3S_EXEC='server --secrets-encryption --tls-san <IP2> --write-kubeconfig-mode 600' sh -

    sudo k3s secrets-encrypt status      # attendu : Enabled
    sudo kubectl -n kube-system rollout status deploy/traefik
    sudo kubectl get crd middlewares.traefik.io

Copier le dossier deploy/k8s sur le serveur 2.

cert-manager et l'émetteur (nom attendu par le chart : letsencrypt-prod) :

    sudo kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.3/cert-manager.yaml
    sudo kubectl -n cert-manager rollout status deploy/cert-manager-webhook
    sed 's/ops@REMPLACER.invalid/ops@<domaine>/' cluster-issuer.yaml | sudo kubectl apply -f -
    sudo kubectl get clusterissuer letsencrypt-prod     # READY=True

Compte de la plateforme, à droits réduits (rbac.yaml explique ses limites) :

    sudo kubectl apply -f rbac.yaml

Bases gérées : CloudNativePG et son plugin de sauvegarde (le plugin demande
cert-manager, installé juste avant). Versions validées sur le cluster de test :

    sudo kubectl apply --server-side -f \
      https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml
    sudo kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager
    sudo kubectl apply -f \
      https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.15.1/manifest.yaml
    sudo kubectl -n cnpg-system rollout status deploy/barman-cloud

Sans eux, la création d'une base échoue (ressource Cluster inconnue) ; le reste
de la plateforme fonctionne.

registries.yaml n'est pas nécessaire : le registre a un certificat public et les
pods reçoivent leurs identifiants par des imagePullSecrets.

À valider sur ce serveur (jamais testé) : BuildKit sans root peut être bloqué par
AppArmor sur Ubuntu 23.10 et suivantes (kernel.apparmor_restrict_unprivileged_userns).
Si un build échoue au démarrage, regarder ce réglage en premier.


5. SERVEUR 1 : INSTALLATION
---------------------------

    sudo mkdir -p /opt/betorun && sudo chown $USER /opt/betorun
    git clone <dépôt kubemanager> /opt/betorun/kubemanager
    cd /opt/betorun/kubemanager
    sudo scripts/generate_prod_secrets.sh /opt/betorun/secrets

Le script écrit .env.prod (600), le stockage objet et le certificat des jetons du
registre, sans rien afficher. Il refuse d'écraser un fichier existant.

    !!! IAM_API_KEY_PEPPER et IAM_STS_KEY_ENCRYPTION_KEY SONT DÉFINITIFS dès le
    !!! premier démarrage. Changer le pepper invalide toutes les clés API. Changer
    !!! la clé STS rend illisibles les clés de signature et les variables chiffrées
    !!! des conteneurs. Sauvegarder /opt/betorun/secrets hors du serveur MAINTENANT.

Compléter /opt/betorun/secrets/.env.prod : remplacer example.com partout, puis
SERVER2_IP, PLATFORM_ADMINS, SMTP_*, MAIL_FROM_EMAIL, ACME_EMAIL et les images.
L'API refuse de démarrer tant qu'il reste une valeur d'exemple.

L'application ne lit que les variables KUBEMANAGER_*. docker-compose.prod.yaml les
alimente depuis .env.prod (DB_PASSWORD devient KUBEMANAGER_DATABASE_PASSWORD, etc.) :
une seule valeur à tenir. Tous les secrets sont en hexadécimal, car le DSN de la
base est construit sans échappement ; seule exception, la clé STS, en base64.

Kubeconfig, depuis le poste qui a accès aux deux serveurs :

    ssh serveur2 'sudo bash make-kubeconfig.sh <IP2>' > /tmp/kubeconfig.betorun
    scp /tmp/kubeconfig.betorun serveur1:/opt/betorun/secrets/kubeconfig && rm /tmp/kubeconfig.betorun
    # sur le serveur 1 : l'image tourne sous l'UID 10001
    sudo chown 10001 /opt/betorun/secrets/kubeconfig && sudo chmod 600 /opt/betorun/secrets/kubeconfig

Images. Toutes viennent de la CI (section 10). À défaut, pour la console, sur une
autre machine, depuis kweb ; les valeurs sont figées dans le JavaScript, donc une
image par domaine :

    docker build \
      --build-arg NEXT_PUBLIC_API_BASE_URL=https://api.<domaine>/kmanager/v1 \
      --build-arg NEXT_PUBLIC_SITES_BASE_DOMAIN=sites.<domaine> \
      -t thekrauss/kweb:<sha> .
    docker push thekrauss/kweb:<sha>

Toutes les commandes suivantes passent par deploy/compose.sh, qui charge les
fichiers d'environnement. Si le dossier n'est pas /opt/betorun/secrets, exporter
BETORUN_SECRETS_DIR.


5 BIS. APPLICATION GITHUB
-------------------------

Facultative : sans elle, les builds depuis un dépôt GitHub sont désactivés, le
reste fonctionne. Créer une application NEUVE (jamais celle du développement),
GitHub, Settings, Developer settings, GitHub Apps, New GitHub App :

  Homepage URL               https://app.<domaine>
  Callback URL               https://app.<domaine>/integrations/github/callback
  Request user authorization (OAuth) during installation : cochée
  Setup URL                  (vide : la Callback URL sert)
  Webhook URL                https://api.<domaine>/kmanager/v1/integrations/github/webhook
  Webhook secret             openssl rand -hex 32 (à garder pour .env.prod)
  Permissions du dépôt       Contents : Read-only ; Metadata : Read-only
  Événements                 Push (installation et installation_repositories
                             arrivent sans case à cocher)
  Where can be installed     Any account

Puis générer un client secret et une clé privée. Sur le serveur 1 :

    install -m 600 -o 10001 <clé>.pem /opt/betorun/secrets/github/app.pem
    rm <clé>.pem

et dans .env.prod : GITHUB_APP_ID, GITHUB_APP_SLUG (nom dans l'URL de l'app),
GITHUB_APP_CLIENT_ID, GITHUB_APP_CLIENT_SECRET, GITHUB_APP_WEBHOOK_SECRET, et
GITHUB_APP_PRIVATE_KEY_FILE=/etc/betorun/github/app.pem. Une configuration à moitié
remplie empêche l'API de démarrer.


6. ORDRE DE DÉMARRAGE
---------------------

a) Base et Keycloak :

    deploy/compose.sh up -d --wait k8sdb keycloak

b) Bootstrap de Keycloak (realm, clients). Rejouable. Il écrit les secrets des
   clients dans keycloak.env sans les afficher :

    deploy/compose.sh --profile bootstrap up keycloak-bootstrap

   Keycloak n'envoie aucun email en production (KEYCLOAK_SMTP_ENABLED=false) : c'est
   le backend qui s'en charge. Il crée aussi le flux betorun-browser de la double
   authentification (KEYCLOAK_MFA_ENABLED=true dans le compose) : le step-up des
   actions sensibles demande acr=gold, sans ce flux il échouerait toujours. Ne jamais lancer scripts/keycloak/bootstrap_platform.sh
   avec ses valeurs par défaut : localhost, MailHog, admin/admin.

c) Tout le reste. compose respecte les dépendances : schéma Temporal, Temporal,
   espace de noms, API (seule à appliquer les migrations), worker, console, Caddy :

    deploy/compose.sh up -d
    deploy/compose.sh ps

Profils, désactivés par défaut. Accès par tunnel SSH uniquement (ports limités à
la boucle locale), par exemple ssh -L 3000:127.0.0.1:3000 serveur1 :

    deploy/compose.sh --profile observability up -d   # Grafana, Prometheus, Loki, Jaeger (~1,5 Go)
    deploy/compose.sh --profile tools up -d           # temporal-ui, MailHog

Pour activer les traces : TRACING_JAEGER_ENDPOINT=jaeger:4317 dans .env.prod.


7. VÉRIFICATIONS
----------------

Serveur 1 :

    deploy/compose.sh ps                 # tout healthy ; les étapes uniques en Exited (0)
    deploy/compose.sh exec api wget -qO- http://127.0.0.1:8080/api/v1/ready
    deploy/compose.sh logs caddy | grep -i 'certificate obtained'

Depuis l'extérieur :

    curl -I  https://app.<domaine>/                       # 200, en-tête HSTS
    curl -sI https://api.<domaine>/api/v1/health          # 200
    curl -sI https://api.<domaine>/metrics                # 404
    curl -sI https://api.<domaine>/api/v1/ready           # 404
    curl -sI -H 'Accept-Encoding: zstd' https://api.<domaine>/kmanager/v1/openapi.json   # content-encoding: zstd
    curl -sI https://registry.<domaine>/v2/               # 401, realm en https://api.<domaine>/...
    curl -sI https://realtime.<domaine>/api               # 404
    # doivent échouer, de l'extérieur et depuis le serveur 2 :
    nc -zw3 <IP1> 5432 ; nc -zw3 <IP1> 7233 ; nc -zw3 <IP1> 8080 ; nc -zw3 <IP1> 9100
    # doit échouer sauf depuis le serveur 1 :
    nc -zw3 <IP2> 6443

Adresse réelle des clients (conditionne les limites de connexion) : se connecter
à la console, ouvrir la page des sessions actives. L'IP affichée doit être la vôtre,
pas 172.30.0.x ni celle du serveur.

Chaîne complète : créer un compte (email reçu ?), déployer un site, un conteneur
avec TLS, un build ; surveiller sur le serveur 2 :

    sudo kubectl get certificate,challenge,order -A

Le solveur HTTP-01 doit passer malgré la redirection : son Ingress a un chemin plus
long, donc un routeur prioritaire ; son pod n'est pas sélectionné par la NetworkPolicy
du chart. Vérifié sur k3s avec un solveur simulé, À CONFIRMER avec le vrai cert-manager.
Le quota du projet peut aussi refuser le pod du solveur s'il est plein : regarder
les événements de l'espace de noms.

Limites Let's Encrypt : sslip.io est partagé avec le monde entier, ses certificats
peuvent être refusés par manque de quota. Si cela arrive, c'est sur les domaines
personnalisés qu'il faut s'appuyer.


7 BIS. WATCHER DU CLUSTER
-------------------------

Clusters suivants : depuis le back-office (Clusters, « Préparer le cluster ») avec
un kubeconfig administrateur utilisé une seule fois ; voir docs/modules/clusters.md,
« Agent de chaque cluster ». Image : WATCHER_IMAGE dans .env.prod. La procédure
ci-dessous reste celle du premier cluster.

Agent du serveur 2 (dépôt watcher) : il remonte les événements des pods à l'API et
publie les clés des jetons Kubernetes. Sans lui, CLUSTER_ID reste vide et l'identité
des workloads est désactivée. JAMAIS TESTÉ entre deux serveurs : seul le rendu du
chart a été vérifié (helm template, le 2026-11-02).

Image : la CI du dépôt watcher publie thekrauss/watcher:<sha> à chaque push sur
main. À défaut, depuis le dépôt watcher, sur une autre machine :

    docker build -t thekrauss/watcher:<sha> .
    docker push thekrauss/watcher:<sha>

Clé : dans la console, Clés de cluster, créer une clé de type « fichier de clé » pour
un identifiant de cluster stable, par exemple preprod. Le fichier n'est affiché
qu'une fois. Le copier sur le serveur 2, puis, l'API devant être joignable :

    sudo kubectl create namespace betorun-system
    sudo kubectl -n betorun-system create secret generic watcher-key-file \
      --from-file=key.json=<fichier>
    rm <fichier>

Installation, depuis le dépôt watcher sur le serveur 2 (Helm 3 à installer) :

    sudo env KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm upgrade --install watcher ./chart \
      -n betorun-system \
      --set cluster.id=preprod \
      --set kubemanager.url=https://api.<domaine>/kmanager/v1/clusters \
      --set kubemanager.keyFileExistingSecret=watcher-key-file \
      --set image.repository=thekrauss/watcher --set image.tag=<sha> \
      --set replicaCount=1

    sudo kubectl -n betorun-system get pods -l app.kubernetes.io/instance=watcher
    sudo kubectl -n betorun-system logs -l app.kubernetes.io/instance=watcher

Puis, sur le serveur 1 : CLUSTER_ID=preprod dans .env.prod, et

    deploy/compose.sh up -d --no-deps api worker

Le watcher ne joint le serveur 1 que par https://api.<domaine> (443) : aucun port de
plus à ouvrir.


7 TER. SITES STATIQUES : STATIC-EDGE DANS LE CLUSTER
----------------------------------------------------

static-edge sert les sites depuis le serveur 2, derrière Traefik. Il ne lit pas
Postgres : il demande à l'API quel site servir (clé du cluster, comme le watcher)
et lit les fichiers par https://s3.<domaine> (identité « edge », lecture seule).
Si l'API ne répond plus, les sites déjà consultés restent servis 15 minutes.

Prérequis : watcher enrôlé (section 7 bis), CLUSTER_ID renseigné, DNS de la
section 2 (*.sites et edge vers <IP2>, s3 vers <IP1>).

a) Certificat joker des sites (DNS-01) : une clé d'API chez l'hébergeur DNS du
   domaine, limitée à la zone. Voir deploy/k8s/cluster-issuer-dns.yaml (exemple
   Cloudflare ; webhook pour OVH) :

    sudo kubectl -n cert-manager create secret generic cloudflare-api-token \
      --from-file=api-token=<fichier>
    rm <fichier>
    sed 's/ops@REMPLACER.invalid/ops@<domaine>/' cluster-issuer-dns.yaml | sudo kubectl apply -f -
    sudo kubectl get clusterissuer letsencrypt-dns     # READY=True

b) Secrets de static-edge. Une SECONDE clé « fichier de clé » pour le même
   identifiant de cluster que le watcher (console, Clés de cluster) : chacun la
   sienne, révocable séparément. Le secret S3 est S3_EDGE_SECRET_KEY du serveur 1 :

    sudo kubectl create namespace betorun-edge
    sudo kubectl -n betorun-edge create secret generic static-edge-key-file \
      --from-file=key.json=<fichier>
    sudo kubectl -n betorun-edge create secret generic static-edge-s3 \
      --from-file=secret-key=<fichier contenant S3_EDGE_SECRET_KEY>
    rm <fichiers>

c) Déploiement (domaine et tag de l'image publiée par la CI) :

    sed -e 's/REMPLACER-DOMAINE/<domaine>/g' -e 's/REMPLACER-SHA/<sha>/' \
      static-edge.yaml | sudo kubectl apply -f -
    sudo kubectl -n betorun-edge rollout status deploy/static-edge
    sudo kubectl -n betorun-edge get certificate sites-wildcard   # READY=True
    curl -sI https://<un-site>.sites.<domaine>

d) Domaines des clients : à chaque domaine vérifié, le worker crée dans
   betorun-edge un Ingress (étiquette betorun.io/edge-domain) et cert-manager son
   certificat HTTP-01 ; ils partent avec le domaine ou le site
   (KUBEMANAGER_SITES_EDGE_INGRESS_ENABLED, déjà dans le compose). Contrôle :

    sudo kubectl -n betorun-edge get ingress -l betorun.io/edge-domain=true

Mise à jour de l'image : la CI la publie mais ne la déploie pas (le serveur 2 n'est
pas joint par la CI) :

    sudo kubectl -n betorun-edge set image deploy/static-edge static-edge=thekrauss/static-edge:<sha>


7 QUATER. CILIUM ET IP DE SORTIE DÉDIÉES (ADR 0014)
---------------------------------------------------

JAMAIS ESSAYÉ À CE JOUR : procédure à valider sur le terrain (préprod, coupure de
quelques minutes : les pods sont recréés sur le nouveau réseau).

Cilium remplace flannel et le kube-proxy de k3s ; sa passerelle de sortie fait
sortir chaque projet par son IP dédiée.

Cluster existant à un nœud (terrain) : bascule sur place, données gardées,
avec relevé avant/après et retour arrière : deploy/k8s/cilium-migrate.sh
(check, puis migrate ; rollback au besoin). Les étapes a) et b) ci-dessous
valent pour un nouveau cluster.

a) k3s sans flannel ni kube-proxy (nouveau cluster, ou réinstallation) :

    curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--flannel-backend=none \
      --disable-network-policy --disable-kube-proxy" sh -

b) Cilium (CLI cilium, sur le serveur) :

    sudo env KUBECONFIG=/etc/rancher/k3s/k3s.yaml cilium install \
      --set kubeProxyReplacement=true \
      --set k8sServiceHost=<IP du serveur> --set k8sServicePort=6443 \
      --set egressGateway.enabled=true --set bpf.masquerade=true
    sudo env KUBECONFIG=/etc/rancher/k3s/k3s.yaml cilium status --wait

c) Droits du compte plateforme : réappliquer deploy/k8s/rbac.yaml (règle
   cilium.io), ou « Préparer le cluster » depuis le back-office.

d) Chaque IP de sortie : l'acheter chez l'hébergeur (IP additionnelle OVH
   rattachée au serveur), la monter sur l'interface du nœud passerelle
   (netplan, en /32), puis dans le back-office : Clusters, le cluster,
   passerelle de sortie « cilium », et ajouter l'IP à la réserve.


8. ADRESSE DES CLIENTS ET RATE LIMIT
------------------------------------

server.trusted_proxies vaut 172.30.0.10 (Caddy) et 172.30.0.11 (console), IP fixes
du sous-réseau 172.30.0.0/24 défini dans le compose. Ne jamais y mettre 0.0.0.0/0 :
un client choisirait son IP et contournerait toutes les limites (l'application le
refuse, comme toute plage plus large qu'un /16).

Ordre dans l'API : proxies de confiance, puis ClientIP, puis rate limit. Sans proxies
corrects, tous les clients partageraient un seul compteur. Défaut : 600 requêtes par
minute et par IP, sur toutes les routes publiques comprises (/registry/token,
/builds/:id/source, /iam/token, webhook GitHub) ; réglable par RATE_LIMIT_PER_MINUTE.

Deux situations partagent une même IP, à surveiller (réponses 429) :
  - les abonnés d'un opérateur mobile derrière un même NAT ;
  - tous les pods et nœuds du serveur 2 : un conteneur client très bavard vers l'API
    pourrait épuiser le compteur utilisé par les pulls d'images et les builds.


9. SAUVEGARDES
--------------

deploy/backup.sh, chaque nuit, vers un stockage objet S3 d'un AUTRE fournisseur que
les serveurs (Backblaze B2 retenu). Tout est chiffré avec age avant de partir.

  Pièce                                  Fréquence    Objet
  rôles Postgres                         chaque jour  pg-globals.sql.age
  chaque base (betorun, keycloak,        chaque jour  pg-<base>.dump.age
    temporal, temporal_visibility)
  /opt/betorun/secrets (.env.prod...)    chaque jour  secrets.tgz.age
  sites des clients (SeaweedFS)          chaque jour  volume-s3data.tgz.age
  registre d'images                      dimanche     volume-registry-data.tgz.age
  certificats de Caddy                   dimanche     volume-caddy-data.tgz.age

Non sauvegardés : Redis (cache), Prometheus, Loki, Grafana. Les disques des
conteneurs clients (PVC du serveur 2) NE SONT PAS sauvegardés.

Rangement : daily/<horodatage>/ du lundi au samedi, weekly/<horodatage>/ le dimanche.
C'est le bucket qui supprime, pas le serveur.

LES CLÉS
  - Chiffrement : seule la clé PUBLIQUE age est sur le serveur. Il peut chiffrer,
    jamais relire. La clé privée reste hors des serveurs (gestionnaire de mots de
    passe et copie hors ligne) : sans elle, aucune restauration possible.
  - Stockage : clé B2 limitée au bucket, en écriture seule (Write Only). Elle ne
    peut ni lire ni supprimer. La clé maîtresse du compte ne va sur aucun serveur.

PRÉPARER LE BUCKET (une fois, interface Backblaze)
  - bucket privé, Object Lock activé ;
  - Object Lock, rétention par défaut : Governance, 7 jours. Même avec toutes les
    clés du serveur, une sauvegarde ne peut pas être effacée avant ;
  - Lifecycle Settings, règles personnalisées :
      préfixe daily/   masquer après 7 jours,  supprimer 1 jour après
      préfixe weekly/  masquer après 28 jours, supprimer 1 jour après
  - Application Keys : une clé Write Only limitée au bucket, pour le serveur.

INSTALLER (serveur 1, en root)

    docker build -t betorun-backup /opt/betorun/kubemanager/deploy/backup

/opt/betorun/secrets/backup.env, droits 600 (copié dans la sauvegarde des secrets) :

    BACKUP_BUCKET=betorun-backups
    BACKUP_S3_ENDPOINT=s3.eu-central-003.backblazeb2.com
    BACKUP_S3_REGION=eu-central-003
    BACKUP_AGE_RECIPIENT=age1...        clé publique
    B2_KEY_ID=...                       clé Write Only
    B2_APPLICATION_KEY=...

Premier essai à la main, puis cron :

    /opt/betorun/kubemanager/deploy/backup.sh
    17 3 * * * /opt/betorun/kubemanager/deploy/backup.sh >> /var/log/betorun-backup.log 2>&1

Une pièce en échec : les autres partent quand même, le code de sortie est 1 et
/var/lib/betorun-backup/last-success n'avance pas. Ce fichier (horodatage UTC) est
celui que les alertes surveillent : plus de 26 heures, la sauvegarde est en panne.
Un jeu incomplet peut contenir une pièce tronquée : age refuse de la déchiffrer.
Restaurer depuis un jeu complet (présent dans le journal avec « terminée »).

Pendant la copie des sites, SeaweedFS est figé (docker pause) : les sites
répondent en retard le temps de copier le volume sur disque local, pas pendant
l'envoi. Prévoir sur /var/lib/betorun-backup la place du volume s3data.

RESTAURER (sur votre poste, jamais testé : à répéter à blanc avant d'en avoir besoin)
Il faut : la clé privée age, une clé B2 Read Only limitée au bucket (à créer pour
l'occasion, puis à supprimer), age, rclone, et Docker sur le serveur.

  1. Lister et télécharger un jeu complet :

    export RCLONE_S3_PROVIDER=Other RCLONE_S3_ENDPOINT=s3.eu-central-003.backblazeb2.com \
      RCLONE_S3_REGION=eu-central-003 RCLONE_S3_ACCESS_KEY_ID=... RCLONE_S3_SECRET_ACCESS_KEY=...
    rclone --config "" lsf :s3:betorun-backups/daily/
    rclone --config "" copy :s3:betorun-backups/daily/<horodatage> ./restore

  2. Déchiffrer :

    for f in restore/*.age; do age -d -i betorun-backup.key -o "${f%.age}" "$f"; done

  3. Secrets : remettre secrets.tgz dans /opt/betorun/secrets (mêmes mots de passe,
     mêmes clés IAM : DÉFINITIFS, sans eux les données sont illisibles).
  4. Postgres sur un volume VIDE : démarrer k8sdb seul. Le script d'initialisation
     recrée rôles et bases à partir de .env.prod. Puis, pour chaque base :

    deploy/compose.sh up -d k8sdb
    deploy/compose.sh exec -T k8sdb sh -c 'pg_restore -U "$POSTGRES_USER" -d "$1" --clean --if-exists' sh <base> < pg-<base>.dump

     pg-globals.sql ne sert que si les rôles diffèrent de ceux du script.
  5. Volumes, AVANT de démarrer leur service (volume vide) :

    docker run --rm -i -v betorun_s3data:/data alpine:3.23.7 tar -C /data -xzf - < volume-s3data.tgz

     Même chose pour registry-data et caddy-data (jeu weekly).
  6. deploy/compose.sh up -d, puis les vérifications de la section 7.

Temporal restauré à une date passée : les workflows en cours depuis sont perdus,
des déploiements peuvent rester à relancer depuis la console. Le script
d'initialisation ne rejoue pas sur un volume déjà rempli : changer un mot de passe
de service demande un ALTER ROLE.


10. MISES À JOUR
----------------

Automatique, à configurer dans chaque dépôt (Settings, Secrets and variables,
Actions) :

  Dépôt        Secrets                                        Variables
  betomanager  DOCKER_USERNAME, DOCKER_PASSWORD,              DEPLOY_ENABLED
               VPS_HOST, VPS_USER, VPS_SSH_KEY
  kweb         les mêmes                                      DEPLOY_ENABLED,
                                                              API_BASE_URL,
                                                              SITES_BASE_DOMAIN
  watcher      DOCKER_USERNAME, DOCKER_PASSWORD               (aucune)

DOCKER_PASSWORD : un jeton d'accès Docker Hub (Read & Write), jamais le mot de
passe du compte. VPS_SSH_KEY : une clé dédiée au déploiement, sans phrase de
passe, dont la clé publique est seule dans authorized_keys de VPS_USER sur le
serveur 1. Pour kweb : API_BASE_URL=https://api.<domaine>/kmanager/v1 et
SITES_BASE_DOMAIN=sites.<domaine>, figés dans l'image de la console.

Chaque push sur main publie les images sous le tag du commit (thekrauss/kubemanager,
thekrauss/static-edge, thekrauss/kweb, thekrauss/watcher). static-edge et le watcher
tournent dans le cluster : ils se mettent à jour à la main (sections 7 bis et 7 ter).
Tant que DEPLOY_ENABLED
n'est pas à true, la CI publie les images mais ne déploie rien. Chaque dépôt ne
remplace que ses lignes dans images.env (CONSOLE_IMAGE pour kweb). Le watcher ne se
déploie pas automatiquement : section 7 bis, avec le tag publié.
Le dépôt doit être cloné dans /opt/betorun/kubemanager et l'utilisateur VPS_USER
doit pouvoir écrire /opt/betorun/secrets/images.env et lire .env.prod.

Manuelle : écrire le tag du commit dans /opt/betorun/secrets/images.env :

    KUBEMANAGER_IMAGE=thekrauss/kubemanager:<sha>

puis, API d'abord (elle applique les migrations) :

    git checkout <sha>
    deploy/compose.sh pull api worker
    deploy/compose.sh up -d --no-deps api
    deploy/compose.sh up -d --no-deps worker

Retour arrière : remettre l'ancien tag. Les migrations ne sont pas annulées : une
sauvegarde avant toute mise à jour qui en contient est indispensable.

!!! VERSION INCOMPATIBLE AVEC LES EXÉCUTIONS EN COURS !!!
Le code des workflows de déploiement de cette version a changé de façon incompatible
avec les exécutions en cours. Aucun workflow workload-deploy-* ou workload-update-*
ne doit tourner au moment de la mise à jour vers cette version. Vérifier avec :

    deploy/compose.sh exec temporal sh -c 'temporal workflow list --address "$(hostname -i | cut -d" " -f1):7233" --query "ExecutionStatus=\"Running\""'

Sans objet pour une première installation.

Images d'infrastructure : versions exactes dans docker-compose.prod.yaml, à monter
une par une, sauvegarde faite. Temporal : temporal-schema rejoue les migrations de
schéma à chaque up. Keycloak est en 25.0.6, comme le développement ; une version plus
récente existe et mérite d'être prévue. Postgres : un changement de version majeure
demande pg_dump et restauration.


11. LIMITES CONNUES
-------------------

- UNE SEULE INSTANCE D'API pour l'instant : la liste de révocation des jetons par JTI
  est locale à chaque processus (cache Ristretto, router/dependencies.go). Avec
  plusieurs instances, une déconnexion ne serait pas vue par les autres.
- Pas de plafond de taille de requête vers les conteneurs des clients sous Traefik.
  Le seul moyen, le middleware buffering (ingress.traefik.maxBodyBytes du chart),
  retient aussi les réponses jusqu'à leur fin. Mesuré sur k3s : un flux SSE arrive
  d'un bloc à la fin. Il reste donc désactivé.
- Les conteneurs clients voient l'adresse de Traefik plutôt que celle du visiteur
  tant que le service Traefik n'est pas en externalTrafficPolicy: Local (non testé).
- Sites : les domaines des clients ont chacun leur certificat HTTP-01 (limite Let's
  Encrypt de 50 nouveaux certificats par semaine et par domaine enregistré du
  client, pas du nôtre) ; les sous-domaines *.sites passent par le joker.
- Ancienne installation (static-edge dans le compose du serveur 1) : le rôle
  Postgres edge_ro reste dans la base ; le supprimer à la main (DROP ROLE edge_ro).
- Le code retombe sur nginx si KUBEMANAGER_KUBERNETES_INGRESS_CLASS manque
  (activities/constants.go) ; le compose de production et la validation l'imposent.
- CLUSTER_ID reste vide tant que le watcher n'est pas enrôlé (section 7 bis) : l'identité
  des workloads est alors désactivée.
