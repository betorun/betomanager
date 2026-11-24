package router

import (
	"context"
	"time"

	"github.com/google/uuid"

	databasesDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	databasesSvc "github.com/thekrauss/kubemanager/internal/modules/databases/service"
	projectSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// Suppression programmée des projets (délai de grâce de 12 h) : arrêt des
// conteneurs et mise en veille des bases à la demande, relance à la
// restauration, purge régulière de ceux dont le délai est écoulé.

// projectPurgeEvery : fréquence de la purge (l'effacement suit l'échéance
// d'au plus cet intervalle).
const projectPurgeEvery = 5 * time.Minute

// wireProjectDeletion : branche la suppression programmée sur le service des
// projets et la garde des écritures. Sans base, l'effacement reste immédiat.
func (a *App) wireProjectDeletion() {
	ps, ok := a.Services.Project.(*projectSvc.ProjectService)
	if !ok || ps == nil || a.DB == nil {
		return
	}
	store, ok := a.Repos.Project.(projectSvc.ProjectDeletionStore)
	if !ok {
		return
	}
	ps.Deletion = store
	if a.Services.Workload != nil {
		ps.Containers = &projectContainers{a: a, svc: a.Services.Workload}
	}
	if a.Controllers != nil && a.Controllers.Databases != nil && a.Controllers.Databases.Service != nil {
		ps.ProjectDatabases = &projectDatabases{svc: a.Controllers.Databases.Service}
	}
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		ps.Notifier = n
	}
	a.Security.Middleware.SetProjectState(ps)
}

// startProjectPurge : effacement des projets dont le délai est écoulé.
func (a *App) startProjectPurge(ctx context.Context) {
	ps, ok := a.Services.Project.(*projectSvc.ProjectService)
	if !ok || ps == nil || ps.Deletion == nil {
		return
	}
	a.goWith(ctx, "project-purge", func(ctx context.Context) {
		t := time.NewTicker(projectPurgeEvery)
		defer t.Stop()
		for {
			ps.PurgeDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

type projectContainers struct {
	a   *App
	svc workloadsSvc.IWorkloadService
}

func (c *projectContainers) StopRunning(ctx context.Context, project uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	if err := c.a.DB.WithContext(ctx).Raw(`SELECT id FROM workloads
		WHERE project_id = ? AND status NOT IN ('STOPPED', 'DELETING', 'DELETED') ORDER BY created_at`, project).
		Scan(&ids).Error; err != nil {
		return nil, err
	}
	var stopped []uuid.UUID
	var firstErr error
	for _, id := range ids {
		if err := c.svc.Stop(ctx, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stopped = append(stopped, id)
	}
	return stopped, firstErr
}

func (c *projectContainers) Start(ctx context.Context, id uuid.UUID) error {
	return c.svc.Start(ctx, id)
}

type projectDatabases struct{ svc *databasesSvc.Service }

func (d *projectDatabases) StopRunning(ctx context.Context, project uuid.UUID) ([]uuid.UUID, error) {
	list, err := d.svc.List(ctx, project)
	if err != nil {
		return nil, err
	}
	var stopped []uuid.UUID
	var firstErr error
	for _, db := range list {
		if db.Status != databasesDomain.StatusReady {
			continue
		}
		id, err := uuid.Parse(db.ID)
		if err != nil {
			continue
		}
		if _, err := d.svc.Stop(ctx, project, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stopped = append(stopped, id)
	}
	return stopped, firstErr
}

func (d *projectDatabases) Start(ctx context.Context, project, id uuid.UUID) error {
	_, err := d.svc.Start(ctx, project, id)
	return err
}
