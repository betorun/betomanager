// Package diskresize : agrandir le disque d'un conteneur (ou d'un service)
// par copie.
//
// Le stockage local-path ne sait pas agrandir un disque. On procède donc par
// copie, conteneur arrêté :
//  1. arrêt du conteneur, attente qu'aucun pod n'utilise le disque ;
//  2. disque temporaire à la nouvelle taille, copie, vérification ;
//  3. le disque d'origine est supprimé puis recréé à la nouvelle taille,
//     sous le même nom et avec les mêmes étiquettes (Helm le reprend) ;
//  4. recopie depuis le disque temporaire, vérification ;
//  5. nouvelle taille enregistrée et conteneur redéployé (relancé s'il
//     tournait) ; le disque temporaire est supprimé.
//
// Rien n'est supprimé sans copie vérifiée. Un échec avant l'étape 3 remet
// tout en l'état ; après, le disque temporaire est gardé et une nouvelle
// demande reprend là où l'opération s'est arrêtée.
package diskresize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/thekrauss/kubemanager/internal/core/background"
	"go.uber.org/zap"
	"gorm.io/gorm"
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

// Workloads : conteneurs (module workloads).
type Workloads interface {
	Get(ctx context.Context, id uuid.UUID) (*Workload, error)
	Stop(ctx context.Context, id uuid.UUID) error
	Start(ctx context.Context, id uuid.UUID) error
	// ApplySize : enregistre la taille et redéploie (le conteneur repart).
	ApplySize(ctx context.Context, id uuid.UUID, size string) error
	// Storage : disque déjà réservé par le projet, et son plafond (octets).
	Storage(ctx context.Context, project uuid.UUID) (used, limit int64, err error)
}

// Claims : disques Kubernetes.
type Claims interface {
	// WaitDetached : plus aucun pod n'utilise le disque.
	WaitDetached(ctx context.Context, ns, claim string) error
	// Size : taille demandée ("" : absent).
	Size(ctx context.Context, ns, claim string) (string, error)
	// CreateTemp : disque temporaire ; il garde les étiquettes du disque
	// d'origine pour pouvoir le recréer.
	CreateTemp(ctx context.Context, ns, temp, from, size string) error
	// Recreate : supprime `claim` (s'il existe) et le recrée à `size`, avec
	// les étiquettes gardées sur `temp`.
	Recreate(ctx context.Context, ns, claim, temp, size string) error
	// Copy : copie vérifiée de `from` vers `to`.
	Copy(ctx context.Context, ns, from, to string, uid, gid int64) error
	Delete(ctx context.Context, ns, claim string) error
}

// Store : opérations enregistrées.
type Store interface {
	Create(ctx context.Context, r *Resize) error
	Save(ctx context.Context, r *Resize) error
	Last(ctx context.Context, workload uuid.UUID) (*Resize, error) // nil : aucune
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

// Service : opérations d'agrandissement.
type Service struct {
	Store     Store
	Workloads Workloads
	Claims    Claims
	Logger    *zap.SugaredLogger
	Now       func() time.Time
	// Sync : l'opération tourne dans l'appel (tests).
	Sync bool
	// Spawner : tâches de fond suivies par l'arrêt (nil : goroutine simple).
	Spawner background.Spawner
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *zap.SugaredLogger {
	if s.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return s.Logger
}

// locks : une opération à la fois par conteneur.
var locks sync.Map

func validSize(size string) bool {
	for _, x := range Sizes {
		if x == size {
			return true
		}
	}
	return false
}

// Start : vérifie la demande et lance l'opération (en arrière-plan).
func (s *Service) Start(ctx context.Context, project, workloadID uuid.UUID, size, actor string) (*Resize, error) {
	if s.Claims == nil || s.Workloads == nil || s.Store == nil {
		return nil, ErrUnavailable
	}
	w, err := s.Workloads.Get(ctx, workloadID)
	if err != nil {
		return nil, err
	}
	if w == nil || w.ProjectID != project {
		return nil, ErrNotFound
	}
	if !w.Persistent {
		return nil, ErrNotPersistent
	}
	size = strings.TrimSpace(size)
	m, _ := locks.LoadOrStore(w.ID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, ErrRunning
	}
	r, resumed, err := s.prepare(ctx, w, size, actor)
	if err == nil {
		if resumed {
			err = s.Store.Save(ctx, r)
		} else {
			err = s.Store.Create(ctx, r)
		}
	}
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	run := func(ctx context.Context) {
		defer mu.Unlock()
		s.run(ctx, w, r)
	}
	if s.Sync {
		run(ctx)
	} else {
		// Suivie par l'arrêt mais pas annulée (voir run) : un arrêt plus
		// long que le délai laisse l'opération « en cours » sans verrou, et
		// la demande suivante la reprend.
		detached := context.WithoutCancel(ctx)
		background.Go(s.Spawner, "disk-resize", func(context.Context) { run(detached) })
	}
	out := *r
	return &out, nil
}

// prepare : nouvelle opération, ou reprise de la précédente. Appelé verrou
// pris : une opération « en cours » sans verrou a été coupée par un
// redémarrage de l'API, elle se reprend.
func (s *Service) prepare(ctx context.Context, w *Workload, size, actor string) (*Resize, bool, error) {
	last, err := s.Store.Last(ctx, w.ID)
	if err != nil {
		return nil, false, err
	}
	if last != nil && last.Status == StatusRunning {
		last.Status = StatusFailed // interrompue
	}
	if last != nil && (last.Resumable() || (last.Status == StatusFailed && last.FinishedAt == nil)) {
		if size != "" && size != last.ToSize {
			return nil, false, ErrResumeSize
		}
		last.Status, last.Error, last.FinishedAt = StatusRunning, "", nil
		return last, true, nil
	}
	if !validSize(size) || bytesOf(size) <= bytesOf(w.StorageSize) {
		return nil, false, ErrInvalidSize
	}
	used, limit, err := s.Workloads.Storage(ctx, w.ProjectID)
	if err != nil {
		return nil, false, err
	}
	if limit > 0 && used-bytesOf(w.StorageSize)+bytesOf(size) > limit {
		return nil, false, ErrQuota
	}
	return &Resize{ID: uuid.New(), WorkloadID: w.ID, ProjectID: w.ProjectID, FromSize: w.StorageSize, ToSize: size,
		Phase: PhaseStopping, Status: StatusRunning, WasStopped: w.Status == "STOPPED", CreatedBy: actor, CreatedAt: s.now()}, false, nil
}

// Last : dernière opération du conteneur (nil : aucune).
func (s *Service) Last(ctx context.Context, project, workloadID uuid.UUID) (*Resize, error) {
	if s.Store == nil {
		return nil, ErrUnavailable
	}
	r, err := s.Store.Last(ctx, workloadID)
	if err != nil || r == nil || r.ProjectID != project {
		return nil, err
	}
	if r.Status == StatusRunning {
		// Plus porté par personne : coupé par un redémarrage de l'API.
		if m, ok := locks.Load(workloadID); !ok || m.(*sync.Mutex).TryLock() {
			if ok {
				m.(*sync.Mutex).Unlock()
			}
			r.Status, r.Error = StatusFailed, ErrInterrupted.Error()
		}
	}
	r.CanResume = r.Resumable() || (r.Status == StatusFailed && r.FinishedAt == nil)
	return r, nil
}

// phase : enregistre la phase AVANT d'agir. Faux si l'enregistrement
// échoue : l'opération s'arrête là, « en cours » sans verrou, et la reprise
// repart de la dernière phase enregistrée — qui décrit alors exactement
// l'état des disques. Continuer sans trace ferait reprendre d'une phase
// antérieure (ex. supprimer le disque temporaire qui porte les données).
func (s *Service) phase(ctx context.Context, r *Resize, p string) bool {
	prev := r.Phase
	r.Phase = p
	if err := s.Store.Save(context.WithoutCancel(ctx), r); err != nil {
		r.Phase = prev
		s.log().Errorw("disk resize: phase not recorded, stopping (resumable)", "resize", r.ID, "phase", p, "error", err)
		return false
	}
	return true
}

// run : déroule les phases à partir de celle enregistrée.
func (s *Service) run(ctx context.Context, w *Workload, r *Resize) {
	ns, claim, temp := w.Namespace, w.Claim(), w.TempClaim()
	fail := func(err error) {
		at := s.now()
		r.Status, r.Error, r.FinishedAt = StatusFailed, err.Error(), &at
		s.log().Warnw("disk resize failed", "workload", w.ID, "phase", r.Phase, "error", err)
		_ = s.Store.Save(context.WithoutCancel(ctx), r)
	}
	// Retour en arrière avant toute suppression : disque d'origine intact.
	rollback := func(err error) {
		if derr := s.Claims.Delete(ctx, ns, temp); derr != nil {
			s.log().Warnw("disk resize: temporary disk left", "workload", w.ID, "error", derr)
		}
		if !r.WasStopped {
			if serr := s.Workloads.Start(ctx, w.ID); serr != nil {
				err = fmt.Errorf("%w (and the container could not be started again: %v)", err, serr)
			}
		}
		fail(err)
	}

	if r.Phase == PhaseStopping || r.Phase == PhaseCopying {
		if !s.phase(ctx, r, PhaseStopping) {
			return
		}
		if w.Status != "STOPPED" {
			if err := s.Workloads.Stop(ctx, w.ID); err != nil {
				fail(fmt.Errorf("the container could not be stopped: %w", err))
				return
			}
		}
		if err := s.Claims.WaitDetached(ctx, ns, claim); err != nil {
			rollback(err)
			return
		}
		if !s.phase(ctx, r, PhaseCopying) {
			return
		}
		if err := s.Claims.Delete(ctx, ns, temp); err != nil { // reste d'un essai précédent
			rollback(err)
			return
		}
		if err := s.Claims.CreateTemp(ctx, ns, temp, claim, r.ToSize); err != nil {
			rollback(err)
			return
		}
		if err := s.Claims.Copy(ctx, ns, claim, temp, w.UID, w.GID); err != nil {
			rollback(err)
			return
		}
		if !s.phase(ctx, r, PhaseSwapping) {
			return
		}
	}
	if r.Phase == PhaseSwapping {
		// Le disque d'origine n'est supprimé que s'il n'a pas déjà la
		// nouvelle taille (reprise après sa recréation).
		if cur, err := s.Claims.Size(ctx, ns, claim); err != nil {
			fail(err)
			return
		} else if cur != r.ToSize {
			if err := s.Claims.Recreate(ctx, ns, claim, temp, r.ToSize); err != nil {
				fail(err)
				return
			}
		}
		if !s.phase(ctx, r, PhaseRestoring) {
			return
		}
	}
	if r.Phase == PhaseRestoring {
		if err := s.Claims.Copy(ctx, ns, temp, claim, w.UID, w.GID); err != nil {
			fail(err)
			return
		}
		if !s.phase(ctx, r, PhaseStarting) {
			return
		}
	}
	if r.Phase == PhaseStarting {
		if err := s.Workloads.ApplySize(ctx, w.ID, r.ToSize); err != nil {
			fail(fmt.Errorf("the disk was resized, but the container could not be redeployed: %w", err))
			return
		}
		if r.WasStopped {
			if err := s.Workloads.Stop(ctx, w.ID); err != nil {
				s.log().Warnw("disk resize: container not stopped again", "workload", w.ID, "error", err)
			}
		}
		if err := s.Claims.Delete(ctx, ns, temp); err != nil {
			s.log().Warnw("disk resize: temporary disk left", "workload", w.ID, "error", err)
		}
	}
	at := s.now()
	r.Phase, r.Status, r.FinishedAt = PhaseDone, StatusCompleted, &at
	if err := s.Store.Save(context.WithoutCancel(ctx), r); err != nil {
		s.log().Warnw("disk resize: result not recorded", "resize", r.ID, "error", err)
	}
}

// bytesOf : « 5Gi » → octets (Gi et Mi seulement).
func bytesOf(q string) int64 {
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

//	Stockage
//
// GormStore : opérations en base.
type GormStore struct{ DB *gorm.DB }

func (g *GormStore) Create(ctx context.Context, r *Resize) error {
	return g.DB.WithContext(ctx).Create(r).Error
}

func (g *GormStore) Save(ctx context.Context, r *Resize) error {
	return g.DB.WithContext(ctx).Save(r).Error
}

func (g *GormStore) Last(ctx context.Context, workload uuid.UUID) (*Resize, error) {
	var r Resize
	err := g.DB.WithContext(ctx).Where("workload_id = ?", workload).Order("created_at DESC").First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}
