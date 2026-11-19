package service

import (
	"context"
	"errors"
	"strings"

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
	// excluded : projets retirés malgré leur organisation (politique de refus).
	ListProjects(ctx context.Context, orgIDs, projectIDs, excluded []uuid.UUID) (*typesproject.ListProjectsResponse, error)
	GetProject(ctx context.Context, projectID string) (*typesproject.ProjectDetail, error)
	UpdateProject(ctx context.Context, in typesproject.UpdateProjectRequest) (*typesproject.ProjectDetail, error)
	// OrganizationQuotas : projets et plafond des organisations données.
	OrganizationQuotas(ctx context.Context, orgIDs []uuid.UUID) ([]typesproject.OrganizationQuota, error)
	// SetMaxProjects : plafond de projets d'une organisation (staff).
	SetMaxProjects(ctx context.Context, orgID uuid.UUID, max int) (*typesproject.OrganizationQuota, error)
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
	// Databases : disques des bases gérées du projet ; nil : non comptés.
	Databases DatabaseStorage
}

// DatabaseStorage : octets de disque réservés par les bases d'un projet.
type DatabaseStorage interface {
	StorageBytes(ctx context.Context, projectID uuid.UUID) (int64, error)
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

// ErrProjectNameTaken : nom déjà utilisé par un autre projet de l'organisation.
var ErrProjectNameTaken = repository.ErrNameTaken

// ErrInvalidOrganization : identifiant d'organisation mal formé.
var ErrInvalidOrganization = errors.New("invalid organization id")

func (s *ProjectService) CreateProject(ctx context.Context, req typesproject.CreateProjectRequest, ownerID string) (*typesproject.ProjectResponse, error) {
	// Limites fixées par la plateforme : la requête n'en porte pas.
	cpuLimit, memoryLimit := DefaultProjectCPU, DefaultProjectMemory
	// La ligne et le rôle owner sont écrits avant la réponse : l'appelant peut
	// lire l'état du projet tout de suite (PROVISIONING), et un nom déjà pris
	// est refusé ici plutôt qu'au milieu du workflow.
	name, err := cleanName(req.Name)
	if err != nil {
		return nil, err
	}
	var orgID uuid.UUID // vide : organisation personnelle du propriétaire
	if req.OrganizationID != "" {
		if orgID, err = uuid.Parse(req.OrganizationID); err != nil {
			return nil, ErrInvalidOrganization
		}
	}
	id := uuid.New()
	projectID := id.String()
	description := strings.TrimSpace(req.Description)
	namespace := utils.NewProjectNamespace(id)
	if err := s.Repos.CreateProject(ctx, &authdomain.Project{
		ID:             id,
		Name:           name,
		Description:    description,
		Namespace:      namespace,
		OrganizationID: orgID,
		CpuLimit:       cpuLimit,
		MemoryLimit:    memoryLimit,
		Status:         utils.ProjectStatusProvisioning,
	}, ownerID); err != nil {
		return nil, err // ErrNameTaken, ErrProjectLimit : classés par le routeur
	}
	input := workflows.CreateProjectInput{
		ProjectID:   projectID,
		Name:        name,
		Namespace:   namespace,
		Description: description,
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
