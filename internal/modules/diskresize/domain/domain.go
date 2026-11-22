package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// bytesOf : « 5Gi » → octets (Gi et Mi seulement).
func BytesOf(q string) int64 {
	q = strings.TrimSpace(q)
	var n int64
	var unit string
	if _, err := fmt.Sscanf(q, "%d%s", &n, &unit); err != nil {
		return 0
	}
	switch unit {
	case "Gi":
		return n << 30
	case "Mi":
		return n << 20
	}
	return 0
}

var (
	ErrNotPersistent = errors.New("this container has no disk")
	ErrInvalidSize   = errors.New("invalid disk size: 1Gi, 2Gi, 5Gi, 10Gi or 20Gi, larger than the current one")
	ErrRunning       = errors.New("a disk resize is already running for this container")
	ErrQuota         = errors.New("quota exceeded: the project's storage limit does not allow this size")
	ErrUnavailable   = errors.New("disk resize is not available on this platform")
	ErrNotFound      = errors.New("container not found")
	ErrInterrupted   = errors.New("the resize was interrupted: ask again to resume it")
	ErrResumeSize    = errors.New("a previous resize stopped halfway: retry it with the same size to finish it")
)

// Phases (dans l'ordre).
const (
	PhaseStopping  = "stopping"
	PhaseCopying   = "copying"
	PhaseSwapping  = "swapping"
	PhaseRestoring = "restoring"
	PhaseStarting  = "starting"
	PhaseDone      = "done"
)

// États.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Sizes : tailles proposées (comme à la création d'un conteneur).
var Sizes = []string{"1Gi", "2Gi", "5Gi", "10Gi", "20Gi"}

// Resize : une opération (table disk_resizes).
type Resize struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	WorkloadID uuid.UUID  `gorm:"type:uuid;not null" json:"workload_id"`
	ProjectID  uuid.UUID  `gorm:"type:uuid;not null" json:"-"`
	FromSize   string     `gorm:"not null" json:"from_size"`
	ToSize     string     `gorm:"not null" json:"to_size"`
	Phase      string     `gorm:"not null" json:"phase"`
	Status     string     `gorm:"not null" json:"status"`
	Error      string     `gorm:"not null" json:"error,omitempty"`
	WasStopped bool       `gorm:"not null" json:"-"`
	CreatedBy  string     `gorm:"not null" json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// CanResume : échec après la suppression du disque d'origine ; une
	// nouvelle demande (même taille) termine l'opération.
	CanResume bool `gorm:"-" json:"can_resume"`
}

func (Resize) TableName() string { return "disk_resizes" }

// Resumable : échec après la suppression du disque d'origine (le disque
// temporaire porte les données) : seule une reprise est possible.
func (r *Resize) Resumable() bool {
	return r.Status == StatusFailed && (r.Phase == PhaseSwapping || r.Phase == PhaseRestoring || r.Phase == PhaseStarting)
}

// Workload : ce que l'opération doit savoir du conteneur.
type Workload struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	Name        string // release Helm ; disque <nom>-pvc
	Namespace   string
	Persistent  bool
	StorageSize string
	Status      string
	UID, GID    int64 // utilisateur et groupe des fichiers
	// Managed : porteur d'un service (Redis…), agrandi par la page Services.
	Managed bool
}

// Claim : nom du disque d'un conteneur (chart standard-app).
func (w Workload) Claim() string { return w.Name + "-pvc" }

// TempClaim : disque temporaire de l'opération.
func (w Workload) TempClaim() string { return w.Name + "-pvc-resize" }
