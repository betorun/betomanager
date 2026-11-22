package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

// DefaultEvery : fréquence des passages.
const DefaultEvery = time.Hour

// Store : dernières mesures.
type Store interface {
	ActiveProjects(ctx context.Context) ([]domain.Project, error)
	Get(ctx context.Context, namespace, pvc string) (*domain.Usage, error) // nil : jamais mesuré
	Save(ctx context.Context, u *domain.Usage) error
	// Prune : retire les lignes des disques disparus de l'espace.
	Prune(ctx context.Context, namespace string, keep []string) error
	ListProject(ctx context.Context, project uuid.UUID) ([]domain.Usage, error)
}

// Targets : conteneurs et bases, et leur arrêt (données gardées).
type Targets interface {
	Workload(ctx context.Context, project uuid.UUID, name string) (*domain.Target, error) // nil : inconnu
	Database(ctx context.Context, project uuid.UUID, name string) (*domain.Target, error)
	StopWorkload(ctx context.Context, id uuid.UUID) error
	StopDatabase(ctx context.Context, project, id uuid.UUID) error
}

// Alerter : e-mail aux propriétaires et éditeurs du projet.
type Alerter interface {
	DiskUsage(ctx context.Context, project, subject uuid.UUID, level int, kind, name string, used, capacity int64)
}

// statusStopped : état d'un conteneur (utils.WorkloadStopped) ou d'une base
// (domain.StatusStopped) arrêtés.
const statusStopped = "STOPPED"

// Service : passages de mesure et règle de dépassement.
type Service struct {
	Store    Store
	Measurer Measurer
	Targets  Targets
	Alerts   Alerter
	Logger   *zap.SugaredLogger
	Now      func() time.Time
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

// Run : un passage peu après le démarrage, puis toutes les `every`.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute): // laisse démarrer le reste du worker
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass : mesure tous les disques des projets actifs.
func (s *Service) Pass(ctx context.Context) {
	projects, err := s.Store.ActiveProjects(ctx)
	if err != nil {
		s.log().Warnw("disk usage: projects lookup failed", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		s.project(ctx, p)
	}
}

func (s *Service) project(ctx context.Context, p domain.Project) {
	disks, err := s.Measurer.Disks(ctx, p.Namespace)
	if err != nil {
		s.log().Warnw("disk usage: disks lookup failed", "namespace", p.Namespace, "error", err)
		return
	}
	keep := make([]string, 0, len(disks))
	for _, d := range disks {
		keep = append(keep, d.PVC)
		if err := s.disk(ctx, p, d); err != nil {
			s.log().Warnw("disk usage: measure failed", "namespace", p.Namespace, "pvc", d.PVC, "error", err)
		}
	}
	if err := s.Store.Prune(ctx, p.Namespace, keep); err != nil {
		s.log().Warnw("disk usage: prune failed", "namespace", p.Namespace, "error", err)
	}
}

func (s *Service) disk(ctx context.Context, p domain.Project, d domain.Disk) error {
	var target *domain.Target
	var err error
	if d.Kind == domain.KindDatabase {
		target, err = s.Targets.Database(ctx, p.ID, d.Name)
	} else {
		target, err = s.Targets.Workload(ctx, p.ID, d.Name)
	}
	if err != nil {
		return err
	}
	if target == nil {
		return nil // disque d'un objet en cours de création ou de suppression
	}
	used, err := s.Measurer.Measure(ctx, d)
	if err != nil {
		return err
	}
	prev, err := s.Store.Get(ctx, d.Namespace, d.PVC)
	if err != nil {
		return err
	}
	u := &domain.Usage{Namespace: d.Namespace, PVC: d.PVC, ProjectID: p.ID, Kind: d.Kind, TargetID: target.ID, TargetName: d.Name,
		CapacityBytes: d.CapacityBytes, UsedBytes: used, Level: domain.LevelOf(used, d.CapacityBytes), MeasuredAt: s.now()}
	prevLevel := domain.LevelOK
	if prev != nil && prev.TargetID == target.ID {
		prevLevel, u.StoppedAt = prev.Level, prev.StoppedAt
	}
	// Un seuil franchi vers le haut : une alerte (le palier 120 a la sienne,
	// à l'arrêt).
	if u.Level > prevLevel && u.Level != domain.LevelStop {
		s.alert(ctx, p.ID, target.ID, u)
	}
	if u.Level == domain.LevelStop {
		if err := s.stop(ctx, p.ID, d.Kind, target, u, prevLevel); err != nil {
			s.log().Warnw("disk usage: stop failed", "kind", d.Kind, "target", target.ID, "error", err)
		}
	}
	return s.Store.Save(ctx, u)
}

// stop : arrête le conteneur ou la base (données gardées), sauf s'il est
// déjà arrêté ou s'il a été relancé il y a moins de StopGrace après un arrêt
// pour disque plein.
func (s *Service) stop(ctx context.Context, project uuid.UUID, kind string, target *domain.Target, u *domain.Usage, prevLevel int) error {
	if target.Status == statusStopped {
		if prevLevel < domain.LevelStop {
			s.alert(ctx, project, target.ID, u) // déjà arrêté par le client : on prévient seulement
		}
		return nil
	}
	if u.StoppedAt != nil && s.now().Sub(*u.StoppedAt) < domain.StopGrace {
		return nil // relancé pour faire de la place : délai de grâce
	}
	var err error
	if kind == domain.KindDatabase {
		err = s.Targets.StopDatabase(ctx, project, target.ID)
	} else {
		err = s.Targets.StopWorkload(ctx, target.ID)
	}
	if err != nil {
		return err
	}
	at := s.now()
	u.StoppedAt = &at
	s.alert(ctx, project, target.ID, u)
	return nil
}

func (s *Service) alert(ctx context.Context, project, subject uuid.UUID, u *domain.Usage) {
	if s.Alerts != nil {
		s.Alerts.DiskUsage(ctx, project, subject, u.Level, u.Kind, u.TargetName, u.UsedBytes, u.CapacityBytes)
	}
}

// List : disques mesurés d'un projet.
func (s *Service) List(ctx context.Context, project uuid.UUID) (*domain.UsageList, error) {
	list, err := s.Store.ListProject(ctx, project)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Usage{}
	}
	return &domain.UsageList{Disks: list}, nil
}

// Measurer : inventaire et mesure des disques d'un espace.
type Measurer interface {
	Disks(ctx context.Context, namespace string) ([]domain.Disk, error)
	Measure(ctx context.Context, d domain.Disk) (int64, error)
}
