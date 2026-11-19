package router

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/diskusage"
)

// diskUsageService : mesure de l'usage réel des disques (local-path ne
// plafonne pas). Sans cluster, seule la lecture des mesures reste possible.
func (a *App) diskUsageService() *diskusage.Service {
	s := &diskusage.Service{Store: &diskusage.GormStore{DB: a.DB}, Targets: &diskTargets{db: a.DB, app: a},
		Logger: a.Logger.With("component", "disk-usage")}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		s.Measurer = &diskusage.KubeMeasurer{Kube: a.K8sProvider.Client, Image: "busybox:1.37.0",
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

func (t *diskTargets) find(ctx context.Context, table string, project uuid.UUID, name string) (*diskusage.Target, error) {
	var out diskusage.Target
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

func (t *diskTargets) Workload(ctx context.Context, project uuid.UUID, name string) (*diskusage.Target, error) {
	return t.find(ctx, "workloads", project, name)
}

func (t *diskTargets) Database(ctx context.Context, project uuid.UUID, name string) (*diskusage.Target, error) {
	return t.find(ctx, "databases", project, name)
}

func (t *diskTargets) StopWorkload(ctx context.Context, id uuid.UUID) error {
	if t.app.Services == nil || t.app.Services.Workload == nil {
		return errors.New("workload service unavailable")
	}
	return t.app.Services.Workload.Stop(ctx, id)
}
