package service

import (
	"context"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// ClusterState : état d'un Cluster, lu dans son status.
type ClusterState struct {
	Exists        bool
	Phase         string // texte de CloudNativePG, ex. « Cluster in healthy state »
	ReadyInstance bool
	// Hibernated : base arrêtée (annotation cnpg.io/hibernation=on).
	Hibernated bool
	Image      string
	// Sauvegardes : configurées (plugin présent) et dernière issue connue.
	BackupsEnabled bool
	LastBackup     *BackupState
}

// BackupState : une sauvegarde complète (objet Backup de CloudNativePG).
type BackupState struct {
	Phase     string    // completed, running, failed…
	StoppedAt time.Time // fin (vide si en cours)
	Error     string
}

// Clusters : ce que le service demande au cluster Kubernetes.
type Clusters interface {
	Create(ctx context.Context, ns string, spec ClusterSpec) error
	State(ctx context.Context, ns, name string) (ClusterState, error)
	Delete(ctx context.Context, ns, name string) error
	// Hibernate : arrête (on) ou relance (off) l'instance ; le disque reste.
	Hibernate(ctx context.Context, ns, name string, on bool) error
	// AppURI : URI de connexion de l'utilisateur « app » (secret <cluster>-app
	// créé par CloudNativePG). Mot de passe compris : ne jamais la renvoyer.
	AppURI(ctx context.Context, ns, name string) (string, error)
}

// ClusterSpec : une base à créer.
type ClusterSpec struct {
	Name       string // nom du Cluster (db-<nom>)
	DatabaseID string
	ProjectID  string
	Storage    string
	// Restore : née des sauvegardes d'une autre base (nil : base vide).
	Restore *RestoreSource
}

// RestoreSource : base dont on lit les sauvegardes, et jusqu'à quand.
type RestoreSource struct {
	Cluster    string     // Cluster source (son ObjectStore <cluster>-store)
	TargetTime *time.Time // nil : le plus récent
}

// BackupTarget : destination S3 des sauvegardes (nil : pas de sauvegarde).
type BackupTarget struct {
	Endpoint, Bucket, Region string
	AccessKey, SecretKey     string
	Retention                string // ex. 7d
	Schedule                 string // cron à 6 champs
}

// ImportJobSpec : un import à lancer.
type ImportJobSpec struct {
	ImportID string
	Cluster  string // Cluster CloudNativePG (db-<nom>)
	// Image : image Postgres de la base (même version de pg_restore).
	Image string
	// FetchImage : image avec curl pour télécharger le dump.
	FetchImage string
	DumpURL    string
	Clean      bool
	// EgressAllow : adresses privées de l'API (même liste que les builds).
	EgressAllow []string
}

// ImportJobState : issue du Job.
type ImportJobState struct {
	Exists bool
	Done   bool
	Failed bool
	// Log : fin du journal de pg_restore (échec seulement).
	Log string
}

// Importer : Jobs d'import (CNPG sur un vrai cluster).
type Importer interface {
	StartImport(ctx context.Context, ns string, spec ImportJobSpec) error
	ImportState(ctx context.Context, ns, importID string) (ImportJobState, error)
}

// Shell : SQL dans une base (CNPG sur un vrai cluster ; nil : indisponible).
type Shell interface {
	Contents(ctx context.Context, ns string, t TaskTarget) (domain.Contents, error)
	// Reset : vide la base. Snapshot non nil : la copie (pg_dump) est d'abord
	// envoyée à l'API ; si l'envoi échoue, rien n'est vidé.
	Reset(ctx context.Context, ns string, t TaskTarget, snapshot *SnapshotUpload) error
	// Dump : copie (pg_dump) envoyée à l'API, sans rien vider.
	Dump(ctx context.Context, ns string, t TaskTarget, upload *SnapshotUpload) error
}

// TaskTarget : la base visée (Cluster CNPG et son image Postgres).
type TaskTarget struct {
	Cluster string
	Image   string
}

// SnapshotUpload : où le Job envoie la copie (URL signée de l'API).
type SnapshotUpload struct {
	URL         string
	FetchImage  string // image avec curl
	EgressAllow []string
}
