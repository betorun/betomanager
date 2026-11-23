package domain

import "time"

type CreateWorkloadRequest struct {
	ProjectID string `json:"project_id" binding:"required" description:"ID du projet parent"`
	Name      string `json:"name" binding:"required,min=3,max=30" description:"Nom du conteneur"`
	Image     string `json:"image" binding:"required" description:"Image Docker (ex: nginx:latest)"`
	Replicas  int    `json:"replicas" default:"1"`

	// Sorte et processus.
	Kind string `json:"kind" description:"Sorte : web (défaut, service HTTP), worker (processus continu sans port), cron (tâche planifiée) ou service (port TCP interne au projet, ex. Redis ; target_port obligatoire)"`
	// Command / Args : commande de démarrage (remplace l'ENTRYPOINT / le CMD
	// de l'image), ex. une image pour le web et le worker.
	Command []string `json:"command,omitempty" description:"Commande de démarrage (remplace l'ENTRYPOINT de l'image)"`
	Args    []string `json:"args,omitempty" description:"Arguments de la commande (remplacent le CMD de l'image)"`
	// Cron : horaire (5 champs), fuseau IANA (défaut UTC), durée maximale.
	Schedule           string `json:"schedule,omitempty" description:"Horaire d'un cron (5 champs, ex. 0 2 * * *, ou @daily)"`
	TimeZone           string `json:"time_zone,omitempty" description:"Fuseau de l'horaire (ex. Africa/Dakar ; défaut UTC)"`
	MaxDurationSeconds int    `json:"max_duration_seconds,omitempty" description:"Durée maximale d'une exécution, en secondes (défaut 3600)"`

	// Resources
	CPURequest    string `json:"cpu_request" default:"100m"`
	MemoryRequest string `json:"memory_request" default:"128Mi"`
	CPULimit      string `json:"cpu_limit" default:"500m"`
	MemoryLimit   string `json:"memory_limit" default:"512Mi"`

	// Networking
	TargetPort  int    `json:"target_port" default:"8080"`
	ServiceType string `json:"service_type" default:"ClusterIP"` //"ClusterIP" ou "LoadBalancer"

	// Storage
	MountPath          string `json:"mount_path" default:"/data"`
	PersistenceEnabled bool   `json:"persistence_enabled" default:"false"`
	StorageSize        string `json:"storage_size" default:"1Gi"`
	StorageClass       string `json:"storage_class" default:"local-path"`

	// Secrets & env
	EnvVars    map[string]string `json:"env_vars"`
	SecretData map[string]string `json:"secret_data"`

	// Security — override du runAsUser/Group (ex: nginx=101, postgres=999).
	// 0 = utilise les defaults du chart (1000).
	RunAsUser  int64 `json:"run_as_user,omitempty"`
	RunAsGroup int64 `json:"run_as_group,omitempty"`

	// ReadOnlyRootFS : verrouille le filesystem root du container (opt-in).
	// Désactivé par défaut — beaucoup d'images courantes écrivent hors /tmp.
	ReadOnlyRootFS bool `json:"read_only_root_fs" default:"false"`

	// Autoscaling (opt-in, sinon replicas fixes).
	Autoscaling *AutoscalingConfig `json:"autoscaling,omitempty"`

	// Disruption budget (activé auto si replicas >= 2 et PDBEnabled).
	PDBEnabled bool `json:"pdb_enabled" default:"false"`

	// TLS ingress via cert-manager.
	TLSEnabled bool `json:"tls_enabled" default:"false"`

	// NoPublicURL : pas d'adresse publique automatique (<nom>-<projet>.<ip>.sslip.io) ;
	// le conteneur reste joignable par ses domaines vérifiés et en interne.
	NoPublicURL bool `json:"no_public_url,omitempty" description:"Sans adresse publique automatique (sslip.io) : seulement les domaines vérifiés et l'adresse interne"`

	// ImageFrom : conteneur du projet dont ce conteneur reprend chaque nouvelle
	// image (worker qui suit l'API : même code, autre commande).
	ImageFrom string `json:"image_from,omitempty" description:"Nom d'un conteneur du projet : chaque nouvelle image de celui-ci est déployée ici aussi"`

	// WaitFor : adresses nom:port du projet attendues avant de démarrer
	// l'application (conteneur d'initialisation) : le réseau d'un nouveau pod
	// n'est ouvert qu'après quelques secondes. Posées au branchement d'une
	// base ou d'un service ; jamais bloquant (60 s au plus par adresse).
	WaitFor []string `json:"wait_for,omitempty" description:"Adresses nom:port du projet à attendre avant de démarrer (ex. cache:6379), 8 au plus ; posées au branchement d'une base ou d'un service"`

	// ManagedBy : module qui gère ce conteneur (jamais lu de la requête).
	ManagedBy string `json:"-"`

	// HealthCommand : sonde d'un worker (pas de port à interroger) : commande
	// lancée dans le conteneur, code 0 = en bonne santé ; sinon il redémarre.
	HealthCommand []string `json:"health_command,omitempty" description:"Worker : commande de santé lancée dans le conteneur (code 0 = sain), ex. [\"cat\",\"/tmp/healthy\"]"`

	// Probes paths (par défaut "/" sur targetPort).
	HealthPath    string `json:"health_path" default:"/"`
	ReadinessPath string `json:"readiness_path" default:"/"`
	// StartupTimeoutSeconds : délai de démarrage toléré avant de relancer le
	// conteneur (web) ; 0 = 150 s, 600 au plus.
	StartupTimeoutSeconds int `json:"startup_timeout_seconds,omitempty" description:"Délai de démarrage toléré, en secondes (défaut 150, 600 au plus), pour une application lente à démarrer"`

	// ServiceAccount : compte de service du projet sous lequel le workload
	// s'exécute ("serviceAccount:<uuid>" ou uuid). Ses pods obtiennent des
	// jetons betorun sans aucune clé. actAs exigé sur le compte.
	ServiceAccount string `json:"service_account,omitempty" description:"Compte de service sous lequel le conteneur s'exécute (actAs requis)"`
}

// AutoscalingConfig : policy HPA simple (CPU + mémoire).
type AutoscalingConfig struct {
	Enabled                           bool  `json:"enabled"`
	MinReplicas                       int32 `json:"min_replicas" default:"1"`
	MaxReplicas                       int32 `json:"max_replicas" default:"10"`
	TargetCPUUtilizationPercentage    int32 `json:"target_cpu_percent" default:"75"`
	TargetMemoryUtilizationPercentage int32 `json:"target_memory_percent,omitempty"`
}

type WorkloadResponse struct {
	WorkloadID string `json:"workload_id"`
	Status     string `json:"status"`
	Namespace  string `json:"namespace"`
	Message    string `json:"message"`
}

type WorkloadStatusResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Phase       string    `json:"phase"`
	Image       string    `json:"image"`
	ExternalURL string    `json:"external_url,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type UpdateWorkloadRequest struct {
	Image string `json:"image"`
	// StartupTimeoutSeconds : nouveau délai de démarrage (absent : inchangé).
	StartupTimeoutSeconds *int   `json:"startup_timeout_seconds" description:"Nouveau délai de démarrage toléré, en secondes (600 au plus)"`
	StorageSize           string `json:"storage_size" description:"Nouvelle taille du disque (ex: 5Gi)"`
	// EnvVars : remplace les variables non secrètes (absent : inchangées).
	EnvVars map[string]string `json:"env_vars"`
	// SecretData : secrets créés ou modifiés ; les autres sont conservés.
	SecretData map[string]string `json:"secret_data"`
	// Champs absents : valeurs en place conservées.
	Replicas    int    `json:"replicas" description:"Nombre de répliques"`
	TargetPort  int    `json:"target_port" description:"Port d'écoute du conteneur"`
	CPULimit    string `json:"cpu_limit" description:"Limite CPU (ex: 500m)"`
	MemoryLimit string `json:"memory_limit" description:"Limite mémoire (ex: 512Mi)"`
	TLSEnabled  *bool  `json:"tls_enabled" description:"Certificat HTTPS"`
	NoPublicURL *bool  `json:"no_public_url" description:"Sans adresse publique automatique (sslip.io)"`
	// ImageFrom : chaîne vide = ne plus suivre.
	ImageFrom *string `json:"image_from" description:"Conteneur dont suivre les nouvelles images (vide : ne plus suivre)"`
	// HealthCommand : liste vide = plus de sonde (worker seulement).
	HealthCommand *[]string `json:"health_command" description:"Worker : commande de santé (liste vide : aucune)"`
	// HealthPath : chemin HTTP des sondes d'un conteneur web (vivacité,
	// démarrage et disponibilité) ; absent : inchangé.
	HealthPath *string `json:"health_path" description:"Conteneur web : chemin de santé interrogé par les sondes (ex. /healthz)"`
	// Processus : absents = inchangés ; liste vide = commande de l'image.
	Command  *[]string `json:"command" description:"Commande de démarrage (liste vide : celle de l'image)"`
	Args     *[]string `json:"args" description:"Arguments (liste vide : ceux de l'image)"`
	Schedule string    `json:"schedule" description:"Nouvel horaire (cron)"`
}

// ── Observabilité ────────────────────────────────────────────────────────

// LiveStatus : snapshot merge DB + K8s runtime pour un workload donné.
type LiveStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Status      string `json:"status"` // DB-side : STARTING/RUNNING/FAILED...
	Phase       string `json:"phase"`  // DB-side : HELM_PREPARING/DEPLOYED/...
	ExternalURL string `json:"external_url,omitempty"`
	// InternalURL : adresse dans le réseau privé du projet (conteneur web) :
	// les autres conteneurs du projet le joignent par elle, sans passer par
	// Internet.
	InternalURL       string      `json:"internal_url,omitempty"`
	Image             string      `json:"image"`
	Replicas          int32       `json:"replicas"`       // desired
	ReadyReplicas     int32       `json:"ready_replicas"` // K8s runtime
	AvailableReplicas int32       `json:"available_replicas"`
	Pods              []PodStatus `json:"pods,omitempty"`
	// Disk : disque persistant, absent sans stockage.
	Disk *DiskStatus `json:"disk,omitempty"`
	// Kind : web, worker, cron ou service.
	Kind string `json:"kind"`
	// ManagedBy : module qui gère le conteneur (« addon/redis/<id> ») ; ses
	// actions sensibles passent par ce module. Absent : conteneur du client.
	ManagedBy string `json:"managed_by,omitempty"`
	// Command / Args : commande de démarrage enregistrée (vide : celle de
	// l'image).
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Cron : état de la tâche planifiée (cron seulement).
	Cron *CronStatus `json:"cron,omitempty"`
	// PullFailure : l'image ne se récupère pas, et pourquoi (absent sinon).
	PullFailure *PullFailure `json:"pull_failure,omitempty"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// CronStatus : état d'une tâche planifiée.
type CronStatus struct {
	Schedule           string     `json:"schedule"`
	TimeZone           string     `json:"time_zone,omitempty"`
	Suspended          bool       `json:"suspended"`
	Running            int        `json:"running"` // exécutions en cours
	LastScheduleTime   *time.Time `json:"last_schedule_time,omitempty"`
	LastSuccessfulTime *time.Time `json:"last_successful_time,omitempty"`
}

// DiskStatus : disque persistant d'un conteneur.
type DiskStatus struct {
	Size string `json:"size"`
	// BackedUp : le disque est couvert par une sauvegarde. Toujours faux tant
	// que la sauvegarde des disques clients n'existe pas : la console le dit.
	BackedUp bool `json:"backed_up"`
}

// PodStatus : état d'un pod K8s (sous-ensemble pertinent pour l'UI).
type PodStatus struct {
	Name         string    `json:"name"`
	Phase        string    `json:"phase"` // Pending, Running, Succeeded, Failed, Unknown
	Ready        bool      `json:"ready"`
	RestartCount int32     `json:"restart_count"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	NodeName     string    `json:"node_name,omitempty"`
	// LastTermination : dernière fin d'exécution du conteneur (raison et code
	// de sortie : OOMKilled / 137, Error / 1…) ; absente s'il n'a jamais fini.
	LastTermination *Termination `json:"last_termination,omitempty"`
}

// Termination : fin d'exécution d'un conteneur.
type Termination struct {
	Reason     string    `json:"reason,omitempty"`
	ExitCode   int32     `json:"exit_code"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// WorkloadMetrics : consommation live (metrics.k8s.io) vs reserved (DB).
// Aggregate sur tous les pods portant le label `km-workload-id=<uuid>`.
type WorkloadMetrics struct {
	WorkloadID string `json:"workload_id"`
	// Actual — mesuré par le Metrics Server (moyenne ~1 min).
	CPUUsage    string `json:"cpu_usage_actual"`    // ex "120m"
	MemoryUsage string `json:"memory_usage_actual"` // ex "256Mi"
	PodCount    int    `json:"pod_count"`
	// Reserved — ce qui a été demandé côté Deploy (DB).
	ReservedCPU    string `json:"reserved_cpu"`    // ex "500m"
	ReservedMemory string `json:"reserved_memory"` // ex "512Mi"
	Replicas       int    `json:"replicas"`
}

// WorkloadEvent : K8s Event filtré sur un workload (via labelSelector).
type WorkloadEvent struct {
	Type      string    `json:"type"`   // Normal | Warning
	Reason    string    `json:"reason"` // Created, Pulled, FailedMount, ...
	Message   string    `json:"message"`
	Object    string    `json:"object"` // ex: "Pod/my-app-abc123"
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}
