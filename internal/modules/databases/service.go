// Package databases : bases de données gérées (Postgres via CloudNativePG).
package databases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Projects : projet par identifiant (espace de noms Kubernetes).
type Projects interface {
	GetProjectByID(ctx context.Context, id string) (*dauth.Project, error)
}

// Repository : stockage des bases.
type Repository interface {
	Create(ctx context.Context, d *domain.Database) error
	List(ctx context.Context, projectID uuid.UUID) ([]domain.Database, error)
	Get(ctx context.Context, projectID, id uuid.UUID) (*domain.Database, error)
	SetStatus(ctx context.Context, id uuid.UUID, status string) error
	Delete(ctx context.Context, id uuid.UUID) error
}

// MaxPerProject : bases par projet.
const MaxPerProject = 5

// Workloads : conteneurs (projet d'appartenance, secret posé avec redéploiement).
type Workloads interface {
	ProjectOf(ctx context.Context, workloadID uuid.UUID) (uuid.UUID, error)
	SetSecret(ctx context.Context, workloadID uuid.UUID, name, value string) (redeploy bool, err error)
}

// Service : cycle de vie des bases. Clusters nil = fonction indisponible.
type Service struct {
	Repo      Repository
	Projects  Projects
	Clusters  Clusters
	Workloads Workloads
	Logger    *zap.SugaredLogger
}

func (s *Service) available() error {
	if s.Clusters == nil {
		return domain.ErrUnavailable
	}
	return nil
}

func (s *Service) namespace(ctx context.Context, projectID uuid.UUID) (string, error) {
	p, err := s.Projects.GetProjectByID(ctx, projectID.String())
	if err != nil {
		return "", err
	}
	if p.Status != utils.ProjectStatusReady {
		return "", fmt.Errorf("project is not ready (%s)", p.Status)
	}
	return utils.ProjectNamespace(p.Name), nil
}

// Create : enregistre la base puis crée son Cluster. Échec côté Kubernetes :
// la ligne passe en FAILED (visible, supprimable) plutôt que de disparaître.
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in domain.CreateDatabaseRequest, creator string) (*domain.DatabaseView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if !domain.ValidName(name) {
		return nil, domain.ErrInvalidName
	}
	size := strings.TrimSpace(in.StorageSize)
	if size == "" {
		size = domain.DefaultStorage
	}
	if !domain.AllowedStorage[size] {
		return nil, domain.ErrInvalidStorage
	}
	existing, err := s.Repo.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, d := range existing {
		if d.Name == name {
			return nil, domain.ErrNameTaken
		}
	}
	if len(existing) >= MaxPerProject {
		return nil, fmt.Errorf("at most %d databases per project", MaxPerProject)
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	d := &domain.Database{ID: uuid.New(), ProjectID: projectID, Name: name, Engine: domain.EnginePostgres,
		StorageSize: size, Status: domain.StatusProvisioning, CreatedBy: creator}
	if err := s.Repo.Create(ctx, d); err != nil {
		return nil, err
	}
	if err := s.Clusters.Create(ctx, ns, ClusterSpec{Name: d.ClusterName(), DatabaseID: d.ID.String(), Storage: size}); err != nil {
		s.Logger.Errorw("database: cluster creation failed", "database", d.ID, "error", err)
		_ = s.Repo.SetStatus(context.WithoutCancel(ctx), d.ID, domain.StatusFailed)
		if errors.Is(err, ErrOperatorMissing) {
			return nil, domain.ErrUnavailable
		}
		return nil, fmt.Errorf("the database could not be created: %w", err)
	}
	v := view(*d, ns, ClusterState{})
	return &v, nil
}

// List : bases du projet, avec leur état en direct.
func (s *Service) List(ctx context.Context, projectID uuid.UUID) ([]domain.DatabaseView, error) {
	rows, err := s.Repo.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.DatabaseView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, d := range rows {
		out = append(out, s.live(ctx, d, ns))
	}
	return out, nil
}

// Get : une base, avec son état en direct.
func (s *Service) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.DatabaseView, error) {
	d, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	v := s.live(ctx, *d, ns)
	return &v, nil
}

// live : état lu sur le Cluster ; le statut en base suit (PROVISIONING →
// READY) pour que les listes restent justes même sans lecture du cluster.
func (s *Service) live(ctx context.Context, d domain.Database, ns string) domain.DatabaseView {
	if s.Clusters == nil || d.Status == domain.StatusDeleting {
		return view(d, ns, ClusterState{})
	}
	st, err := s.Clusters.State(ctx, ns, d.ClusterName())
	if err != nil {
		s.Logger.Warnw("database: state unavailable", "database", d.ID, "error", err)
		return view(d, ns, ClusterState{})
	}
	next := d.Status
	switch {
	case st.ReadyInstance:
		next = domain.StatusReady
	case !st.Exists && d.Status != domain.StatusFailed && time.Since(d.CreatedAt) > time.Minute:
		next = domain.StatusFailed // Cluster disparu
	case st.Exists && d.Status == domain.StatusReady:
		next = domain.StatusProvisioning // instance redémarrée, pas encore prête
	}
	if next != d.Status {
		if err := s.Repo.SetStatus(ctx, d.ID, next); err == nil {
			d.Status = next
		}
	}
	return view(d, ns, st)
}

// Delete : le Cluster, ses disques et sa politique réseau, puis la ligne.
// Les données sont perdues (action sensible : double authentification).
func (s *Service) Delete(ctx context.Context, projectID, id uuid.UUID) error {
	if err := s.available(); err != nil {
		return err
	}
	d, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return err
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return err
	}
	if err := s.Repo.SetStatus(ctx, d.ID, domain.StatusDeleting); err != nil {
		return err
	}
	if err := s.Clusters.Delete(ctx, ns, d.ClusterName()); err != nil {
		_ = s.Repo.SetStatus(context.WithoutCancel(ctx), d.ID, domain.StatusFailed)
		return fmt.Errorf("the database could not be deleted: %w", err)
	}
	return s.Repo.Delete(ctx, d.ID)
}

// Attach : pose l'URI de connexion de la base dans une variable SECRÈTE du
// conteneur (redéployé). Le conteneur doit être du même projet ; l'URI ne
// sort jamais de l'API. Les droits (connect sur la base, variables du
// conteneur) sont vérifiés avant l'appel.
func (s *Service) Attach(ctx context.Context, projectID, id, workloadID uuid.UUID, variable string) (*domain.AttachResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if s.Workloads == nil {
		return nil, domain.ErrUnavailable
	}
	if variable == "" {
		variable = domain.DefaultVariable
	}
	if !domain.ValidVariable(variable) {
		return nil, domain.ErrInvalidVariable
	}
	owner, err := s.Workloads.ProjectOf(ctx, workloadID)
	if err != nil || owner != projectID {
		return nil, domain.ErrWorkloadNotFound
	}
	d, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if v := s.live(ctx, *d, ns); v.Status != domain.StatusReady {
		return nil, domain.ErrNotReady
	}
	uri, err := s.Clusters.AppURI(ctx, ns, d.ClusterName())
	if err != nil {
		if errors.Is(err, errAppSecretMissing) {
			return nil, domain.ErrNotReady
		}
		return nil, err
	}
	redeploy, err := s.Workloads.SetSecret(ctx, workloadID, variable, uri)
	if err != nil {
		return nil, err
	}
	return &domain.AttachResponse{WorkloadID: workloadID.String(), Variable: variable, Redeploy: redeploy}, nil
}

func view(d domain.Database, ns string, st ClusterState) domain.DatabaseView {
	return domain.DatabaseView{
		ID: d.ID.String(), Name: d.Name, Engine: d.Engine, Version: imageVersion(st.Image),
		StorageSize: d.StorageSize, Status: d.Status, Phase: st.Phase,
		Host: d.ClusterName() + "-rw." + ns + ".svc", Port: domain.Port,
		Database: domain.DatabaseName, User: domain.UserName, CreatedAt: d.CreatedAt,
	}
}

// imageVersion : « ghcr.io/cloudnative-pg/postgresql:17.6 » → « 17.6 ».
func imageVersion(image string) string {
	if i := strings.LastIndexByte(image, ':'); i >= 0 && !strings.Contains(image[i:], "/") {
		return image[i+1:]
	}
	return ""
}

// ── Postgres ───────────────────────────────────────────────────────────────

// GormRepository : stockage Postgres.
type GormRepository struct{ DB *gorm.DB }

func (r *GormRepository) Create(ctx context.Context, d *domain.Database) error {
	err := r.DB.WithContext(ctx).Create(d).Error
	if err != nil && strings.Contains(err.Error(), "databases_project_name") {
		return domain.ErrNameTaken // création concurrente du même nom
	}
	return err
}

func (r *GormRepository) List(ctx context.Context, projectID uuid.UUID) ([]domain.Database, error) {
	var out []domain.Database
	err := r.DB.WithContext(ctx).Where("project_id = ?", projectID).Order("created_at").Find(&out).Error
	return out, err
}

func (r *GormRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.Database, error) {
	var d domain.Database
	err := r.DB.WithContext(ctx).Where("project_id = ? AND id = ?", projectID, id).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	return &d, err
}

func (r *GormRepository) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	return r.DB.WithContext(ctx).Model(&domain.Database{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()}).Error
}

func (r *GormRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Database{}).Error
}
