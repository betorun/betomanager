package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	"github.com/thekrauss/kubemanager/internal/modules/projects/workflows"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	workloadRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
)

type IProjectService interface {
	CreateProject(ctx context.Context, req typesproject.CreateProjectRequest, ownerID string) (*typesproject.ProjectResponse, error)
	DeleteProject(ctx context.Context, projectID string) (*typesproject.ProjectResponse, error)
	GetProjectStatus(ctx context.Context, projectID string) (*typesproject.ProjectStatusResponse, error)
	GetMetrics(ctx context.Context, projectID string) (*typesproject.NamespaceMetrics, error)
	// ListProjects : projets accessibles, donnés par l'IAM (organisations et
	// projets où l'appelant détient resourcemanager.projects.get).
	ListProjects(ctx context.Context, orgIDs, projectIDs []uuid.UUID) (*typesproject.ListProjectsResponse, error)
	// SetLimits : plafonds du projet (staff de la plateforme uniquement).
	SetLimits(ctx context.Context, projectID string, in typesproject.ProjectLimits) (*typesproject.ProjectLimitsResponse, error)
}

var _ IProjectService = (*ProjectService)(nil)

type ProjectService struct {
	Distributor  temporal.TaskDistributor
	Config       *configs.GlobalConfig
	Logger       *zap.SugaredLogger
	Repos        repository.ProjectRepository
	K8sClient    *kubernetes.Clientset
	K8sConfig    *rest.Config
	WorkloadRepo workloadRepo.WorkloadRepository
	// Images : place des images du projet au registre ; nil si les builds
	// ne sont pas activés.
	Images ImageUsage
}

// ImageUsage : octets d'images d'un projet et son quota (0 : sans quota).
type ImageUsage interface {
	Usage(ctx context.Context, project uuid.UUID) (used, quota int64, err error)
}

func NewProjectService(
	dist temporal.TaskDistributor,
	cfg *configs.GlobalConfig,
	log *zap.SugaredLogger,
	repo repository.ProjectRepository,
	k8s *kubernetes.Clientset,
	k8sCfg *rest.Config,
	workloadRepo workloadRepo.WorkloadRepository,
) IProjectService {
	return &ProjectService{
		Distributor:  dist,
		Config:       cfg,
		Logger:       log.With("service", "ProjectService"),
		Repos:        repo,
		K8sClient:    k8s,
		K8sConfig:    k8sCfg,
		WorkloadRepo: workloadRepo,
	}
}

// Limites par défaut d'un projet (identiques aux défauts de la table projects).
const (
	DefaultProjectCPU    = "2000m"
	DefaultProjectMemory = "4Gi"
)

// ErrProjectNameTaken : nom déjà utilisé par un autre projet.
var ErrProjectNameTaken = errors.New("a project with this name already exists")

func (s *ProjectService) CreateProject(ctx context.Context, req typesproject.CreateProjectRequest, ownerID string) (*typesproject.ProjectResponse, error) {
	// Limites fixées par la plateforme : la requête n'en porte pas.
	cpuLimit, memoryLimit := DefaultProjectCPU, DefaultProjectMemory
	// La ligne et le rôle owner sont écrits avant la réponse : l'appelant peut
	// lire l'état du projet tout de suite (PROVISIONING), et un nom déjà pris
	// est refusé ici plutôt qu'au milieu du workflow.
	id := uuid.New()
	projectID := id.String()
	if err := s.Repos.CreateProject(ctx, &authdomain.Project{
		ID:          id,
		Name:        req.Name,
		Description: req.Description,
		CpuLimit:    cpuLimit,
		MemoryLimit: memoryLimit,
		Status:      utils.ProjectStatusProvisioning,
	}, ownerID); err != nil {
		if errors.Is(err, repository.ErrNameTaken) {
			return nil, ErrProjectNameTaken
		}
		return nil, err
	}
	input := workflows.CreateProjectInput{
		ProjectID:   projectID,
		Name:        req.Name,
		Description: req.Description,
		OwnerID:     ownerID,
		CpuLimit:    cpuLimit,
		MemoryLimit: memoryLimit,
	}

	handle, err := s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDCreatePrefix + projectID},
		workflows.CreateProjectWorkflow, input,
	)
	if err != nil {
		s.Logger.Errorw("Failed to start project workflow", "error", err)
		// Sans workflow, personne ne finirait le projet : on retire la ligne.
		if dErr := s.Repos.DeleteProject(context.WithoutCancel(ctx), projectID); dErr != nil {
			s.Logger.Errorw("Failed to remove project after workflow start error", "projectID", projectID, "error", dErr)
		}
		return nil, err
	}

	return &typesproject.ProjectResponse{
		ProjectID:  projectID,
		WorkflowID: handle.WorkflowID,
		Status:     utils.ProjectStatusProvisioning,
		Message:    "the project is currently being created on the cluster.",
	}, nil
}

func (s *ProjectService) DeleteProject(ctx context.Context, projectID string) (*typesproject.ProjectResponse, error) {
	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if err != nil {
		s.Logger.Errorw("Project not found for deletion", "projectID", projectID, "error", err)
		return nil, err
	}

	handle, err := s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDDeletePrefix + projectID},
		workflows.DeleteProjectWorkflowName, projectID, project.Name,
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
