package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	"github.com/thekrauss/kubemanager/internal/modules/projects/workflows"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Suppression programmée d'un projet : la demande arrête tout sans rien
// effacer ; l'effacement définitif n'a lieu qu'après DeletionGrace, sauf
// restauration d'ici là.

// DefaultDeletionGrace : délai avant l'effacement définitif.
const DefaultDeletionGrace = 12 * time.Hour

// purgeBatch : projets effacés au plus par passage de la purge.
const purgeBatch = 20

var (
	// ErrProjectHasVPS : un VPS lié au projet doit être supprimé d'abord.
	ErrProjectHasVPS = errors.New("this project still has VPS: delete them first, then delete the project")
	// ErrProjectNotDeletable : projet en création, ou déjà en cours d'effacement.
	ErrProjectNotDeletable = errors.New("this project cannot be deleted right now: wait until it is ready")
	// ErrProjectNotPendingDeletion : rien à restaurer.
	ErrProjectNotPendingDeletion = errors.New("this project is not scheduled for deletion")
	// ErrProjectPendingDeletion : toute modification attend la restauration.
	ErrProjectPendingDeletion = errors.New("this project is scheduled for deletion: restore it before changing anything")
)

// ProjectDeletionStore : accès en base de la suppression programmée.
type ProjectDeletionStore = repository.DeletionStore

// ProjectContainers : conteneurs d'un projet (service des conteneurs).
type ProjectContainers interface {
	// StopRunning : arrête ceux qui tournent ; rend ceux qui ont été arrêtés.
	StopRunning(ctx context.Context, project uuid.UUID) ([]uuid.UUID, error)
	Start(ctx context.Context, id uuid.UUID) error
}

// ProjectDatabases : bases gérées d'un projet (mise en veille).
type ProjectDatabases interface {
	// StopRunning : met en veille celles qui tournent ; rend celles-là.
	StopRunning(ctx context.Context, project uuid.UUID) ([]uuid.UUID, error)
	Start(ctx context.Context, project, id uuid.UUID) error
}

// DeletionNotifier : prévient les propriétaires (e-mail, journal d'organisation).
type DeletionNotifier interface {
	ProjectDeletionScheduled(ctx context.Context, project uuid.UUID, name string, at time.Time, by string)
	ProjectRestored(ctx context.Context, project uuid.UUID, name string, by string)
}

func (s *ProjectService) grace() time.Duration {
	if s.DeletionGrace > 0 {
		return s.DeletionGrace
	}
	return DefaultDeletionGrace
}

func (s *ProjectService) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func scheduledResponse(id string, at *time.Time) *typesproject.ProjectResponse {
	return &typesproject.ProjectResponse{
		ProjectID: id, Status: utils.ProjectStatusPendingDeletion, DeletionScheduledAt: at,
		Message: "the project is stopped and will be deleted for good at the scheduled time; it can be restored until then",
	}
}

// DeleteProject : programme l'effacement (ou l'exécute tout de suite si la
// suppression programmée n'est pas branchée). Idempotent.
func (s *ProjectService) DeleteProject(ctx context.Context, projectID, actor string) (*typesproject.ProjectResponse, error) {
	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.Deletion == nil {
		return s.purge(ctx, project)
	}
	switch project.Status {
	case utils.ProjectStatusPendingDeletion:
		return scheduledResponse(projectID, project.DeletionScheduledAt), nil
	case utils.ProjectStatusDeleting:
		return &typesproject.ProjectResponse{ProjectID: projectID, Status: utils.ProjectStatusDeleting,
			WorkflowID: workflows.WorkflowIDDeletePrefix + projectID, Message: "the project is being deleted"}, nil
	}
	// Un VPS lié ferait échouer l'effacement à sa dernière étape, après la
	// perte du reste : refusé avant.
	if n, err := s.Deletion.ActiveVPS(ctx, project.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, ErrProjectHasVPS
	}
	at := s.clock().Add(s.grace()).UTC()
	ok, err := s.Deletion.ScheduleDeletion(ctx, project.ID, at, actor)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrProjectNotDeletable
	}
	// Marqué d'abord : plus rien ne peut redémarrer pendant l'arrêt.
	stopped := dauth.DeletionStopped{}
	if s.Containers != nil {
		ids, err := s.Containers.StopRunning(ctx, project.ID)
		if err != nil {
			s.Logger.Warnw("project deletion: containers not all stopped", "project", project.ID, "error", err)
		}
		stopped.Workloads = ids
	}
	if s.ProjectDatabases != nil {
		ids, err := s.ProjectDatabases.StopRunning(ctx, project.ID)
		if err != nil {
			s.Logger.Warnw("project deletion: databases not all stopped", "project", project.ID, "error", err)
		}
		stopped.Databases = ids
	}
	if err := s.Deletion.SetDeletionStopped(ctx, project.ID, stopped); err != nil {
		s.Logger.Errorw("project deletion: stopped resources not recorded", "project", project.ID, "error", err)
	}
	audit.Annotate(ctx, "deletion_scheduled_at", at.Format(time.RFC3339))
	if s.Notifier != nil {
		s.Notifier.ProjectDeletionScheduled(ctx, project.ID, project.Name, at, actor)
	}
	s.Logger.Infow("project deletion scheduled", "project", project.ID, "at", at,
		"stopped_workloads", len(stopped.Workloads), "stopped_databases", len(stopped.Databases))
	return scheduledResponse(projectID, &at), nil
}

// RestoreProject : annule une suppression programmée et relance ce que la
// demande avait arrêté.
func (s *ProjectService) RestoreProject(ctx context.Context, projectID, actor string) (*typesproject.ProjectDetail, error) {
	if s.Deletion == nil {
		return nil, ErrProjectNotPendingDeletion
	}
	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	stopped, ok, err := s.Deletion.CancelDeletion(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrProjectNotPendingDeletion
	}
	if stopped != nil {
		for _, id := range stopped.Databases {
			if s.ProjectDatabases != nil {
				if err := s.ProjectDatabases.Start(ctx, project.ID, id); err != nil {
					s.Logger.Warnw("project restore: database not restarted", "project", project.ID, "database", id, "error", err)
				}
			}
		}
		for _, id := range stopped.Workloads {
			if s.Containers != nil {
				if err := s.Containers.Start(ctx, id); err != nil {
					s.Logger.Warnw("project restore: container not restarted", "project", project.ID, "workload", id, "error", err)
				}
			}
		}
	}
	if s.Notifier != nil {
		s.Notifier.ProjectRestored(ctx, project.ID, project.Name, actor)
	}
	s.Logger.Infow("project restored", "project", project.ID)
	return s.GetProject(ctx, projectID)
}

// PurgeDue : efface les projets dont le délai de grâce est écoulé. Appelée
// régulièrement ; une restauration passée entre-temps l'emporte.
func (s *ProjectService) PurgeDue(ctx context.Context) {
	if s.Deletion == nil {
		return
	}
	due, err := s.Deletion.DueDeletions(ctx, s.clock(), purgeBatch)
	if err != nil {
		s.Logger.Errorw("project purge: due deletions not read", "error", err)
		return
	}
	for i := range due {
		p := &due[i]
		ok, err := s.Deletion.MarkDeleting(ctx, p.ID)
		if err != nil || !ok {
			if err != nil {
				s.Logger.Errorw("project purge: not marked deleting", "project", p.ID, "error", err)
			}
			continue
		}
		if _, err := s.purge(ctx, p); err != nil {
			s.Logger.Errorw("project purge: deletion not started", "project", p.ID, "error", err)
			continue
		}
		s.Logger.Infow("project purge: deletion started", "project", p.ID)
	}
}

// purge : lance l'effacement définitif (workflow de suppression).
func (s *ProjectService) purge(ctx context.Context, project *dauth.Project) (*typesproject.ProjectResponse, error) {
	projectID := project.ID.String()
	handle, err := s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDDeletePrefix + projectID},
		workflows.DeleteProjectWorkflowName, projectID, project.Namespace,
	)
	if temporal.IsAlreadyStarted(err) {
		// Deuxième demande : la suppression est déjà en cours.
		handle, err = temporal.WorkflowHandle{WorkflowID: workflows.WorkflowIDDeletePrefix + projectID}, nil
	}
	if err != nil {
		s.Logger.Errorw("Failed to start delete workflow", "error", err)
		return nil, err
	}
	return &typesproject.ProjectResponse{
		ProjectID:  projectID,
		WorkflowID: handle.WorkflowID,
		Status:     utils.ProjectStatusDeleting,
		Message:    "The project deletion and cleanup has been initiated.",
	}, nil
}

// PendingDeletion : garde des écritures (projet en suppression programmée).
func (s *ProjectService) PendingDeletion(ctx context.Context, project uuid.UUID) (bool, error) {
	if s.Deletion == nil {
		return false, nil
	}
	return s.Deletion.PendingDeletion(ctx, project)
}
