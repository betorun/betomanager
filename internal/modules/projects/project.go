package projects

import (
	"context"

	"github.com/gin-gonic/gin"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"

	typeiam "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/service"
)

type IProjectController interface {
	CreateProject(c *gin.Context, in *typeiam.CreateProjectRequest) (*typeiam.ProjectResponse, error)
	GetProjectStatus(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectStatusResponse, error)
	DeleteProject(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectResponse, error)
	GetProjectMetrics(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.NamespaceMetrics, error)
	ListProjects(c *gin.Context) (*typeiam.ListProjectsResponse, error)
}

// ProjectAccess : où l'appelant détient une permission (iam.Engine).
type ProjectAccess interface {
	Holdings(ctx context.Context, p iam.Principal, perm iam.Permission) (iam.Holdings, error)
}

type ProjectHandler struct {
	ProjectService service.IProjectService
	Access         ProjectAccess
}

func NewProjectHandlers(ps service.IProjectService, access ProjectAccess) *ProjectHandler {
	return &ProjectHandler{ProjectService: ps, Access: access}
}

// ListProjects : projets que l'appelant peut voir (resourcemanager.projects.get
// sur le projet ou sur son organisation). Un compte de service ne voit que
// les projets où il a été autorisé.
func (h *ProjectHandler) ListProjects(c *gin.Context) (*typeiam.ListProjectsResponse, error) {
	p, ok := security.PrincipalFromContext(c)
	if !ok {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "authentication required")
	}
	if h.Access == nil {
		return nil, betoerrors.New(betoerrors.CodeInternal, "project listing unavailable")
	}
	held, err := h.Access.Holdings(c.Request.Context(), p, iam.ProjectsGet)
	if err != nil {
		return nil, err
	}
	return h.ProjectService.ListProjects(c.Request.Context(), held.Organizations, held.Projects)
}

func (h *ProjectHandler) CreateProject(c *gin.Context, in *typeiam.CreateProjectRequest) (*typeiam.ProjectResponse, error) {
	// Fix #5 : clé "userID" (set par middleware/security) — avant on lisait
	// "user_id" qui renvoyait nil, puis .(string) panic type assertion.
	// Propriétaire : identifiant interne (celui des droits IAM), pas
	// l'identifiant Keycloak du jeton.
	owner, ok := security.InternalUserID(c)
	if !ok {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "complete your profile before creating a project")
	}
	return h.ProjectService.CreateProject(c.Request.Context(), *in, owner.String())
}

type GetProjectStatusRequest struct {
	ProjectID string `path:"id" description:"ID du projet"`
}

func (h *ProjectHandler) GetProjectStatus(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectStatusResponse, error) {
	return h.ProjectService.GetProjectStatus(c.Request.Context(), in.ProjectID)
}

func (h *ProjectHandler) DeleteProject(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectResponse, error) {
	return h.ProjectService.DeleteProject(c.Request.Context(), in.ProjectID)
}

func (h *ProjectHandler) GetProjectMetrics(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.NamespaceMetrics, error) {
	return h.ProjectService.GetMetrics(c.Request.Context(), in.ProjectID)
}
