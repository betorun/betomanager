// Package databases : bases de données gérées (Postgres via CloudNativePG).
package databases

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/api/resource"

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

// AttachmentStore : branchements enregistrés (absent : non tracés).
type AttachmentStore interface {
	SaveAttachment(ctx context.Context, a *domain.Attachment) error
	ListAttachments(ctx context.Context, databaseID uuid.UUID) ([]domain.Attachment, error)
}

// WorkloadNamer : nom d'un conteneur (facultatif, pour tracer un branchement).
type WorkloadNamer interface {
	NameOf(ctx context.Context, workloadID uuid.UUID) (string, error)
}

// Workloads : conteneurs (projet d'appartenance, secret posé avec redéploiement).
type Workloads interface {
	ProjectOf(ctx context.Context, workloadID uuid.UUID) (uuid.UUID, error)
	SetSecret(ctx context.Context, workloadID uuid.UUID, name, value string) (redeploy bool, err error)
	// WaitFor : le conteneur attendra host:port avant de démarrer (réseau
	// d'un nouveau pod ouvert après quelques secondes).
	WaitFor(ctx context.Context, workloadID uuid.UUID, target string) error
}

// Service : cycle de vie des bases. Clusters nil = fonction indisponible.
type Service struct {
	Repo      Repository
	Projects  Projects
	Clusters  Clusters
	Workloads Workloads
	// Imports : import de dumps (nil : indisponible).
	Imports *ImportConfig
	// Attachments : branchements tracés (nil : la carte n'en montre aucun).
	Attachments AttachmentStore
	// Shell : lecture et remise à zéro d'une base (nil : indisponibles).
	Shell Shell
	// Snapshots : copies prises avant une remise à zéro (nil : pas de remise à zéro).
	Snapshots SnapshotStore
	// Archives : copies finales avant suppression par la plateforme (nil :
	// la plateforme ne supprime aucune base).
	Archives ArchiveStore
	Logger   *zap.SugaredLogger
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
	return p.Namespace, nil
}

// Create : enregistre la base puis crée son Cluster. Échec côté Kubernetes :
// la ligne passe en FAILED (visible, supprimable) plutôt que de disparaître.
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in domain.CreateDatabaseRequest, creator string) (*domain.DatabaseView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	size := strings.TrimSpace(in.StorageSize)
	if size == "" {
		size = domain.DefaultStorage
	}
	if !domain.AllowedStorage[size] {
		return nil, domain.ErrInvalidStorage
	}
	return s.provision(ctx, projectID, strings.TrimSpace(in.Name), size, creator, nil, nil)
}

// Restore : NOUVELLE base née des sauvegardes de la base id, jusqu'à
// target (nil : le plus récent). La source n'est jamais modifiée ; on
// bascule ensuite les conteneurs avec Attach.
func (s *Service) Restore(ctx context.Context, projectID, id uuid.UUID, name string, target *time.Time, creator string) (*domain.DatabaseView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if target != nil && target.After(time.Now()) {
		return nil, domain.ErrInvalidTarget
	}
	src, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	st, err := s.Clusters.State(ctx, ns, src.ClusterName())
	if err != nil {
		return nil, err
	}
	if !st.BackupsEnabled || st.LastBackup == nil || !strings.EqualFold(st.LastBackup.Phase, "completed") {
		return nil, domain.ErrNoBackup
	}
	if target != nil && !st.LastBackup.StoppedAt.IsZero() && target.Before(st.LastBackup.StoppedAt.Add(-retentionGuard)) {
		// Antérieur à toute sauvegarde conservée probable : CloudNativePG
		// échouerait plus tard, sans message clair. On ne bloque pas
		// (des sauvegardes plus anciennes peuvent exister), on journalise.
		s.Logger.Infow("database: restore target older than the last backup", "database", id, "target", target)
	}
	from := src.ID
	return s.provision(ctx, projectID, strings.TrimSpace(name), src.StorageSize, creator,
		&RestoreSource{Cluster: src.ClusterName(), TargetTime: target}, &from)
}

// retentionGuard : seuil du journal ci-dessus (indicatif).
const retentionGuard = 7 * 24 * time.Hour

func (s *Service) provision(ctx context.Context, projectID uuid.UUID, name, size, creator string, restore *RestoreSource, from *uuid.UUID) (*domain.DatabaseView, error) {
	if !domain.ValidName(name) {
		return nil, domain.ErrInvalidName
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
		StorageSize: size, Status: domain.StatusProvisioning, CreatedBy: creator, RestoredFrom: from}
	if restore != nil {
		d.RestoreTarget = restore.TargetTime
	}
	if err := s.Repo.Create(ctx, d); err != nil {
		return nil, err
	}
	spec := ClusterSpec{Name: d.ClusterName(), DatabaseID: d.ID.String(), ProjectID: projectID.String(), Storage: size, Restore: restore}
	if err := s.Clusters.Create(ctx, ns, spec); err != nil {
		s.Logger.Errorw("database: cluster creation failed", "database", d.ID, "error", err)
		_ = s.Repo.SetStatus(context.WithoutCancel(ctx), d.ID, domain.StatusFailed)
		if errors.Is(err, ErrOperatorMissing) || errors.Is(err, ErrBackupPluginMissing) || errors.Is(err, ErrBackupsDisabled) {
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
		v := s.live(ctx, d, ns)
		s.enrich(ctx, &v, d.ID)
		out = append(out, v)
	}
	return out, nil
}

// enrich : branchements et dernier import (au mieux : une lecture en échec
// laisse la carte sans ces informations).
func (s *Service) enrich(ctx context.Context, v *domain.DatabaseView, id uuid.UUID) {
	v.Attachments = []domain.Attachment{}
	if s.Attachments != nil {
		if list, err := s.Attachments.ListAttachments(ctx, id); err == nil {
			v.Attachments = list
		} else {
			s.warn("database attachments unavailable", "database", id, "error", err)
		}
	}
	if s.Imports.ready() {
		if list, err := s.Imports.Store.ListImports(ctx, id, 1); err == nil && len(list) > 0 {
			v.LastImport = &list[0]
		}
	}
	v.ResetSnapshot = s.snapshotView(ctx, id)
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
	s.enrich(ctx, &v, d.ID)
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
	s.dropSnapshot(ctx, d.ID)
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
	if err := s.Workloads.WaitFor(ctx, workloadID, d.ClusterName()+"-rw:"+strconv.Itoa(domain.Port)); err != nil && s.Logger != nil {
		s.Logger.Warnw("database: wait-for not set", "workload", workloadID, "error", err)
	}
	redeploy, err := s.Workloads.SetSecret(ctx, workloadID, variable, uri)
	if err != nil {
		return nil, err
	}
	s.recordAttachment(ctx, id, workloadID, variable)
	return &domain.AttachResponse{WorkloadID: workloadID.String(), Variable: variable, Redeploy: redeploy}, nil
}

func view(d domain.Database, ns string, st ClusterState) domain.DatabaseView {
	v := domain.DatabaseView{
		ID: d.ID.String(), Name: d.Name, Engine: d.Engine, Version: imageVersion(st.Image),
		StorageSize: d.StorageSize, Status: d.Status, Phase: st.Phase,
		Host: d.ClusterName() + "-rw." + ns + ".svc", Port: domain.Port,
		Database: domain.DatabaseName, User: domain.UserName, CreatedAt: d.CreatedAt,
		Backups: backupsView(st),
	}
	if d.RestoredFrom != nil {
		v.RestoredFrom = d.RestoredFrom.String()
		v.RestoreTarget = d.RestoreTarget
	}
	return v
}

func backupsView(st ClusterState) domain.Backups {
	b := domain.Backups{Enabled: st.BackupsEnabled}
	if st.LastBackup != nil {
		b.LastStatus = st.LastBackup.Phase
		if !st.LastBackup.StoppedAt.IsZero() {
			t := st.LastBackup.StoppedAt
			b.LastAt = &t
		}
	}
	return b
}

// imageVersion : « ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie » → « 18.6 ».
func imageVersion(image string) string {
	i := strings.LastIndexByte(image, ':')
	if i < 0 || strings.Contains(image[i:], "/") {
		return ""
	}
	tag := image[i+1:]
	if j := strings.IndexByte(tag, '-'); j > 0 {
		tag = tag[:j]
	}
	return tag
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

// recordAttachment : trace le branchement (au mieux : la variable est posée,
// une trace manquante ne doit pas faire échouer l'opération).
func (s *Service) recordAttachment(ctx context.Context, databaseID, workloadID uuid.UUID, variable string) {
	if s.Attachments == nil {
		return
	}
	name := workloadID.String()
	if n, ok := s.Workloads.(WorkloadNamer); ok {
		if got, err := n.NameOf(ctx, workloadID); err == nil && got != "" {
			name = got
		}
	}
	a := &domain.Attachment{DatabaseID: databaseID, WorkloadID: workloadID, WorkloadName: name, Variable: variable, CreatedAt: time.Now()}
	if err := s.Attachments.SaveAttachment(ctx, a); err != nil {
		s.warn("database attachment not recorded", "database", databaseID, "workload", workloadID, "error", err)
	}
}

func (r *GormRepository) SaveAttachment(ctx context.Context, a *domain.Attachment) error {
	return r.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(a).Error
}

// ListAttachments : branchements vers des conteneurs toujours présents.
func (r *GormRepository) ListAttachments(ctx context.Context, databaseID uuid.UUID) ([]domain.Attachment, error) {
	out := []domain.Attachment{}
	err := r.DB.WithContext(ctx).Table("database_attachments AS a").Select("a.*").
		Joins("JOIN workloads w ON w.id = a.workload_id").
		Where("a.database_id = ? AND w.status NOT IN ?", databaseID, []string{"DELETED", "DELETING"}).
		Order("a.workload_name, a.variable").Scan(&out).Error
	return out, err
}

// StorageBytes : disques demandés par les bases du projet (vue d'ensemble).
func (r *GormRepository) StorageBytes(ctx context.Context, projectID uuid.UUID) (int64, error) {
	var sizes []string
	if err := r.DB.WithContext(ctx).Model(&domain.Database{}).Where("project_id = ?", projectID).Pluck("storage_size", &sizes).Error; err != nil {
		return 0, err
	}
	var total int64
	for _, sz := range sizes {
		if q, err := resource.ParseQuantity(sz); err == nil {
			total += q.Value()
		}
	}
	return total, nil
}
