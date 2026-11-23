package router

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	databasesRepo "github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	databasesSvc "github.com/thekrauss/kubemanager/internal/modules/databases/service"
	"github.com/thekrauss/kubemanager/internal/modules/diskusage"
	diskusageDomain "github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
	diskusageRepo "github.com/thekrauss/kubemanager/internal/modules/diskusage/repository"
	diskusageSvc "github.com/thekrauss/kubemanager/internal/modules/diskusage/service"
)

// diskUsageService : mesure de l'usage réel des disques (local-path ne
// plafonne pas). Sans cluster, seule la lecture des mesures reste possible.
func (a *App) diskUsageService() *diskusageSvc.Service {
	s := &diskusageSvc.Service{Store: &diskusageRepo.GormStore{DB: a.DB}, Targets: &diskTargets{db: a.DB, app: a},
		Logger: a.Logger.With("component", "disk-usage")}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		s.Measurer = &diskusageRepo.KubeMeasurer{Kube: k8sprovider.ByNamespace(a.kube()), Image: "busybox:1.37.0",
			Timeout: 2 * time.Minute, Poll: 2 * time.Second}
	}
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		s.Alerts = n
	}
	return s
}

func (a *App) newDiskUsageController() *diskusage.Controller {
	return &diskusage.Controller{Service: a.diskUsageService()}
}

// diskTargets : conteneurs et bases par nom dans un projet.
type diskTargets struct {
	db  *gorm.DB
	app *App
}

func (t *diskTargets) find(ctx context.Context, table string, project uuid.UUID, name string) (*diskusageDomain.Target, error) {
	var out diskusageDomain.Target
	err := t.db.WithContext(ctx).Table(table).Select("id, status").
		Where("project_id = ? AND name = ?", project, name).Take(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *diskTargets) Workload(ctx context.Context, project uuid.UUID, name string) (*diskusageDomain.Target, error) {
	return t.find(ctx, "workloads", project, name)
}

func (t *diskTargets) Database(ctx context.Context, project uuid.UUID, name string) (*diskusageDomain.Target, error) {
	return t.find(ctx, "databases", project, name)
}

// StopDatabase : hibernation de la base (même chemin que la console).
func (t *diskTargets) StopDatabase(ctx context.Context, project, id uuid.UUID) error {
	if t.app.K8sProvider == nil || t.app.K8sProvider.Config == nil {
		return errors.New("cluster unavailable")
	}
	s := &databasesSvc.Service{Repo: &databasesRepo.GormRepository{DB: t.db}, Projects: t.app.Repos.Project,
		Clusters: &databasesRepo.CNPG{Clients: t.app.cnpgClients()}, Logger: t.app.Logger}
	_, err := s.Stop(ctx, project, id)
	return err
}

func (t *diskTargets) StopWorkload(ctx context.Context, id uuid.UUID) error {
	if t.app.Services == nil || t.app.Services.Workload == nil {
		return errors.New("workload service unavailable")
	}
	return t.app.Services.Workload.Stop(ctx, id)
}
