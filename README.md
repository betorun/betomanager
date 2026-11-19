# kubemanager

Méthode,Route,Description,Mode d'exécution

1. Service Auth & Utilisateurs
POST,/auth/register,Créer un compte utilisateur,Sync (DB)
POST,/auth/login,Connexion (Retourne JWT),Sync (DB)
POST,/auth/refresh,Rafraîchir le token,Sync
GET,/users/me,Profil utilisateur courant,Sync
PUT,/users/me/password,Changer mot de passe,Sync
GET,/users/api-keys,Lister les clés API (pour CLI),Sync

La Suite : Le Workflow de Création de Projet
Maintenant que Temporal fonctionne, on va implémenter la vraie logique métier : Créer un Namespace Kubernetes via Temporal.

On va créer le workflow ProjectCreationWorkflow.

Voici le plan d'attaque :

Activity 1 : Créer le Namespace dans K8s.

Activity 2 : Créer les ResourceQuotas (CPU/RAM limit).

Activity 3 : Mettre à jour le statut en DB (provisioning -> active).

Workflow : Orchestrer tout ça avec des retries automatiques.

2. Gestion des Projets (Namespaces)
Le "Projet"  correspondra à un Namespace Kubernetes.

GET,/projects,Lister les projets de l'utilisateur,Sync (DB)
POST,/projects,Créer un nouveau projet,Temporal Workflow (Crée Namespace + RBAC + Quotas)
GET,/projects/:id,Détails et statuts (Health),Sync (K8s)
DELETE,/projects/:id,Supprimer un projet,Temporal Workflow (Nettoyage ressources)
GET,/projects/:id/metrics,CPU/RAM global du projet,Sync (Prometheus)

3. Gestion des Applications (Workloads)
Gère les Deployments et Services.

GET,/projects/:id/apps,Lister les applications,Sync (K8s Cache/Informer)
POST,/projects/:id/apps,Déployer une nouvelle app (Image Docker),Temporal Workflow (Create Deployment + Service)
GET,/apps/:id,"Détail d'une app (Status, Image)",Sync
POST,/apps/:id/scale,Changer le nombre de réplicas,Temporal Workflow (Scale Deployment)
POST,/apps/:id/restart,Redémarrage forcé (Rollout restart),Temporal Workflow
PUT,/apps/:id/image,Mettre à jour l'image Docker (CD),Temporal Workflow
DELETE,/apps/:id,Supprimer l'application,Temporal Workflow
GET,/apps/:id/logs,Logs en temps réel,WebSocket (Stream K8s)

4. Configuration & Environnement
Gère les ConfigMaps et Secrets.

Méthode,Route,Description,Mode d'exécution
GET,/apps/:id/env,Lire les variables d'env,Sync (K8s)
PUT,/apps/:id/env,Mettre à jour les variables,Temporal Workflow (Update Secret + Redeploy Pods)

5. Networking & Domaines
Gère les objets Ingress et Cert-Manager.

Méthode,Route,Description,Mode d'exécution
GET,/apps/:id/domains,Lister les domaines liés,Sync
POST,/apps/:id/domains,Ajouter un domaine perso (ex: mon-site.com),Temporal Workflow (Update Ingress)
DELETE,/apps/:id/domains/:domain,Détacher un domaine,Temporal Workflow
POST,/apps/:id/ssl,Forcer la régénération SSL,Temporal Workflow

6. Marketplace (Helm)
Pour les bases de données et services tiers.

GET,/marketplace/catalog,"Lister les services dispos (Redis, PG, etc.)",Sync (JSON local ou DB)
POST,/projects/:id/addons,Installer un service (ex: Redis),Temporal Workflow (Helm Install)
GET,/projects/:id/addons,Lister les services installés,Sync (Helm List)
DELETE,/addons/:id,Désinstaller un service,Temporal Workflow (Helm Uninstall)

7. Suivi des Opérations (Async)
Indispensable car vous utilisez Temporal. Le frontend doit pouvoir poller l'état d'une action longue.

GET,/operations/:workflow_id,"Statut d'une tâche (Running, Completed, Failed)",Sync (Temporal Client)



.
├── api                         # 📄 Definitions Protobuf / Swagger / OpenAPI
│   └── proto
│       └── v1
├── cmd                         # 🚀 Points d'entrée de l'application
│   ├── gateway                 # Le Serveur API (REST)
│   │   └── main.go             # -> Initialise Gin, ton Router, et les Modules
│   └── worker                  # Le Worker Temporal
│       └── main.go             # -> Enregistre les Activités et Workflows
├── config                      # ⚙️ Chargement des variables d'env (Viper/Godotenv)
│   ├── config.go
│   └── config.yaml
├── internal                    # 🔒 Code privé de l'application
│   ├── infrastructure          # 🧱 Briques techniques partagées (Pas de métier ici !)
│   │   ├── database            # Connexion Postgres (Gorm/Sqlc)
│   │   ├── k8s                 # Client-go & Helm init
│   │   │   └── client.go
│   │   ├── temporal            # Client Temporal init
│   │   │   └── client.go
│   │   └── router              # 🚦 TON FRAMEWORK ROUTER CUSTOM
│   │       ├── types.go        # (Tes structs Route, RouteGroup)
│   │       └── wrapper.go      # (Tes fonctions NewRootGroup, RegisterSchema)
│   │
│   └── modules                 # 📦 Tes Domaines Métiers (Le cœur du système)
│       ├── auth                # --- Module Auth ---
│       │   ├── domain          # Structs (User, Role)
│       │   ├── repository      # SQL (UserRepo)
│       │   ├── service         # Logique (Login, HashPassword) - Pas de Temporal ici
│       │   └── delivery
│       │       └── http        # Handlers API
│       │           ├── handler.go
│       │           └── routes.go # (Utilise ton wrapper.go)
│       │
│       ├── projects            # --- Module Projets (Namespaces) ---
│       │   ├── domain          # Structs (Project, Quotas)
│       │   ├── repository      # SQL (Project metadata)
│       │   ├── workflows       # ⚡️ Logique métier Temporal (CreateProjectWorkflow)
│       │   ├── activities      # ⚡️ Appels K8s (CreateNamespaceActivity)
│       │   └── delivery
│       │       └── http
│       │           ├── handler.go
│       │           └── routes.go
│       │
│       └── workloads           # --- Module Apps (Deployment/Svc/Ingress) ---
│           ├── domain          # Structs (Application, DeploymentRequest)
│           ├── workflows       # (DeployAppWorkflow, RollbackWorkflow)
│           ├── activities      # (HelmInstallActivity, CheckPodStatusActivity)
│           └── delivery
│               └── http
│                   ├── handler.go
│                   └── routes.go
│
├── deploy                      # Charts Helm pour déployer 
├── go.mod
└── makefile

3. Les Données de Démarrage (Seed)
Dans ton code (ou une migration), tu dois insérer les rôles par défaut. C'est ce que font tous les providers Cloud.

Exemple de Permissions :

project:view, project:delete, project:edit

k8s:workload:create, k8s:workload:delete, k8s:logs:view

k8s:shell:exec (Dangereux, réservé aux admins)

Exemple de Rôles :

Viewer : A juste les permissions *:view.

Developer : *:view + k8s:workload:* (Peut déployer mais pas supprimer le projet).

Owner : Toutes les permissions.

package domain

const (
    PermProjectRead   = "project:read"
    PermProjectDelete = "project:delete"
    PermAppDeploy     = "app:deploy"
    PermAppLogs       = "app:logs"
)


curl -X POST "http://localhost:8080/kmanager/v1/auth/register" \
     -H "Content-Type: application/json" \
     -d '{
           "email": "admin@kubemanager.io",
           "password": "ton_password_secure",
           "full_name": "Admin Manager"
         }'

curl -X POST "http://localhost:8080/kmanager/v1/auth/login" \
     -H "Content-Type: application/json" \
     -d '{
           "identifier": "admin@kubemanager.io",
           "password": "ton_password_secure"
         }'

curl -X POST "http://localhost:8080/kmanager/v1/projects" \
     -H "Authorization: Bearer MET_TON_TOKEN_ICI" \
     -H "Content-Type: application/json" \
     -d '{
           "name": "alpha-project",
           "description": "Mon premier cluster de test sur VPS",
           "cpu_limit": "500m",
           "memory_limit": "512Mi"
         }'

/--------------------------------------------------------------/

➜  ~ curl -X POST "http://localhost:8080/kmanager/v1/auth/register" \
     -H "Content-Type: application/json" \
     -d '{
           "email": "admin@kubemanager.io",
           "password": "ton_password_secure",
           "full_name": "Admin Manager"
         }'
{
    "id": "71763a1d-b9ed-4b0e-95d1-9b08df473b83",
    "email": "admin@kubemanager.io",
    "full_name": "Admin Manager",
    "created_at": "2026-01-01T09:47:51.945845281Z"
}%          

Si votre application est...,Utilisez le workload...
Un site web ou une API (interchangeable),Deployment
Une base de données (besoin d'identité fixe),StatefulSet
Une tâche unique (ex: script de nettoyage),Job
Une tâche planifiée (ex: backup quotidien),CronJob
Un agent système (ex: logs sur chaque serveur),DaemonSet

Temporal UI	http://localhost:8233	Pour voir tes workflows en cours et l'historique des activités.
Jaeger UI	http://localhost:16686	Pour le tracing distribué et voir la latence de tes requêtes.
Grafana	http://localhost:3000	Ton dashboard central (Metrics + Logs Loki).
Prometheus	http://localhost:9090	Pour explorer tes métriques brutes.
Kubemanager API	http://localhost:8080	Le point d'entrée de ton application Go.


Un détail technique pour ton Excalidraw
Sur ton schéma, assure-toi de bien noter la distinction suivante pour le Watcher :

Il lit les infos depuis l'API Server (via le service interne kubernetes.default).

Il écrit les événements vers Restate (via Internet/VPN).

Le petit conseil en plus : Pour que ton Watcher parle à Restate, utilise le SDK Restate dans ton code Go. Au lieu de faire un simple POST HTTP, tu appelleras un Virtual Object Restate. Exemple : restate.ObjectClient("ProjectMonitor", projectID).Send("onEvent", eventData).


1. La gestion de l'état (Statefulness)
Si le Watcher envoie 10 alertes de crash en 30 secondes :

Le Manager (API classique) : Il est souvent "stateless". Pour savoir si c'est la 1ère ou la 10ème alerte, il doit faire un appel à la base de données (SELECT count...), incrémenter, et comparer. C'est lourd et sujet à des Race Conditions (si deux alertes arrivent en même temps).

Restate (Virtual Objects) : Il possède un "objet virtuel" par ProjectID. L'état est encapsulé. Restate garantit que les événements pour un même projet sont traités l'un après l'autre. Il garde en mémoire : "J'en suis à 3 crashs". C'est extrêmement rapide et fiable.

2. La résilience et les "Retries"
Le réseau entre ton VPS (Watcher) et ton local (Manager) peut vaciller.

Le Manager : Si ton Manager est occupé ou redémarre, le Watcher va recevoir une erreur HTTP 500 ou un timeout. Le Watcher devra alors gérer lui-même la logique complexe de "Retry" (attendre, réessayer, ne pas saturer le serveur).

Restate : Il agit comme un buffer intelligent. Si ton Manager est temporairement indisponible, Restate garde l'événement en attente et réessaiera d'appeler la logique de réaction dès que possible. Tu ne perds aucun événement critique.

3. Le découplage "Ce qui se passe" vs "Ce qu'on fait"
Le Watcher est "bête" : Sa seule mission est de dire : "Je vois ça".

Restate est le "cerveau tactique" : Il décide : "Si je vois ça ET que l'utilisateur n'est pas en train de faire une maintenance, ALORS je demande au Manager de rollback".

Le Manager est l'exécuteur : Il possède les droits d'écriture et les clés de sécurité pour modifier l'infrastructure.