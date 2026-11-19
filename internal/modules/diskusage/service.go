package diskusage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DefaultEvery : fréquence des passages.
const DefaultEvery = time.Hour

// Project : un projet actif et son espace.
type Project struct {
	ID        uuid.UUID
	Namespace string
}

// Store : dernières mesures.
type Store interface {
	ActiveProjects(ctx context.Context) ([]Project, error)
	Get(ctx context.Context, namespace, pvc string) (*Usage, error) // nil : jamais mesuré
	Save(ctx context.Context, u *Usage) error
	// Prune : retire les lignes des disques disparus de l'espace.
	Prune(ctx context.Context, namespace string, keep []string) error
	ListProject(ctx context.Context, project uuid.UUID) ([]Usage, error)
}

// Target : conteneur ou base qui porte un disque.
type Target struct {
	ID     uuid.UUID
	Status string
}

// Targets : conteneurs et bases, et l'arrêt d'un conteneur.
type Targets interface {
	Workload(ctx context.Context, project uuid.UUID, name string) (*Target, error) // nil : inconnu
	Database(ctx context.Context, project uuid.UUID, name string) (*Target, error)
	StopWorkload(ctx context.Context, id uuid.UUID) error
}

// Alerter : e-mail aux propriétaires et éditeurs du projet.
type Alerter interface {
	DiskUsage(ctx context.Context, project, subject uuid.UUID, level int, kind, name string, used, capacity int64)
}

// workloadStopped : état d'un conteneur arrêté (utils.WorkloadStopped).
const workloadStopped = "STOPPED"

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

func (s *Service) project(ctx context.Context, p Project) {
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

func (s *Service) disk(ctx context.Context, p Project, d Disk) error {
	var target *Target
	var err error
	if d.Kind == KindDatabase {
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
	u := &Usage{Namespace: d.Namespace, PVC: d.PVC, ProjectID: p.ID, Kind: d.Kind, TargetID: target.ID, TargetName: d.Name,
		CapacityBytes: d.CapacityBytes, UsedBytes: used, Level: LevelOf(used, d.CapacityBytes), MeasuredAt: s.now()}
	prevLevel := LevelOK
	if prev != nil && prev.TargetID == target.ID {
		prevLevel, u.StoppedAt = prev.Level, prev.StoppedAt
	}
	// Un seuil franchi vers le haut : une alerte (le palier 120 a la sienne,
	// à l'arrêt, pour les conteneurs).
	if u.Level > prevLevel && !(u.Level == LevelStop && d.Kind == KindWorkload) {
		s.alert(ctx, p.ID, target.ID, u)
	}
	if u.Level == LevelStop && d.Kind == KindWorkload {
		if err := s.stop(ctx, p.ID, target, u, prevLevel); err != nil {
			s.log().Warnw("disk usage: stop failed", "workload", target.ID, "error", err)
		}
	}
	return s.Store.Save(ctx, u)
}

// stop : arrête le conteneur (données gardées), sauf s'il est déjà arrêté ou
// s'il a été relancé il y a moins de StopGrace après un arrêt pour disque plein.
func (s *Service) stop(ctx context.Context, project uuid.UUID, target *Target, u *Usage, prevLevel int) error {
	if target.Status == workloadStopped {
		if prevLevel < LevelStop {
			s.alert(ctx, project, target.ID, u) // déjà arrêté par le client : on prévient seulement
		}
		return nil
	}
	if u.StoppedAt != nil && s.now().Sub(*u.StoppedAt) < StopGrace {
		return nil // relancé pour faire de la place : délai de grâce
	}
	if err := s.Targets.StopWorkload(ctx, target.ID); err != nil {
		return err
	}
	at := s.now()
	u.StoppedAt = &at
	s.alert(ctx, project, target.ID, u)
	return nil
}

func (s *Service) alert(ctx context.Context, project, subject uuid.UUID, u *Usage) {
	if s.Alerts != nil {
		s.Alerts.DiskUsage(ctx, project, subject, u.Level, u.Kind, u.TargetName, u.UsedBytes, u.CapacityBytes)
	}
}

// List : disques mesurés d'un projet.
func (s *Service) List(ctx context.Context, project uuid.UUID) (*UsageList, error) {
	list, err := s.Store.ListProject(ctx, project)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []Usage{}
	}
	return &UsageList{Disks: list}, nil
}

// GormStore : mesures en base.
type GormStore struct{ DB *gorm.DB }

func (g *GormStore) ActiveProjects(ctx context.Context) ([]Project, error) {
	var out []Project
	err := g.DB.WithContext(ctx).Table("projects").Select("id, namespace").
		Where("status IN ? AND namespace <> '' AND deleted_at IS NULL", []string{"READY", "ACTIVE"}).Order("created_at").Scan(&out).Error
	return out, err
}

func (g *GormStore) Get(ctx context.Context, namespace, pvc string) (*Usage, error) {
	var u Usage
	err := g.DB.WithContext(ctx).Where("namespace = ? AND pvc = ?", namespace, pvc).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &u, err
}

func (g *GormStore) Save(ctx context.Context, u *Usage) error {
	return g.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(u).Error
}

func (g *GormStore) Prune(ctx context.Context, namespace string, keep []string) error {
	q := g.DB.WithContext(ctx).Where("namespace = ?", namespace)
	if len(keep) > 0 {
		q = q.Where("pvc NOT IN ?", keep)
	}
	return q.Delete(&Usage{}).Error
}

func (g *GormStore) ListProject(ctx context.Context, project uuid.UUID) ([]Usage, error) {
	var out []Usage
	err := g.DB.WithContext(ctx).Where("project_id = ?", project).Order("kind, target_name").Find(&out).Error
	return out, err
}
