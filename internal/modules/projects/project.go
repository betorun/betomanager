package projects

import (
	"context"
	"errors"

	"github.com/google/uuid"

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
	SetProjectLimits(c *gin.Context, in *SetProjectLimitsRequest) (*typeiam.ProjectLimitsResponse, error)
	GetProject(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectDetail, error)
	UpdateProject(c *gin.Context, in *typeiam.UpdateProjectRequest) (*typeiam.ProjectDetail, error)
	SetOrganizationLimits(c *gin.Context, in *SetOrganizationLimitsRequest) (*typeiam.OrganizationQuota, error)
}

// ProjectAccess : où l'appelant détient une permission (iam.Engine).
type ProjectAccess interface {
	Holdings(ctx context.Context, p iam.Principal, perm iam.Permission) (iam.Holdings, error)
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

type ProjectHandler struct {
	ProjectService service.IProjectService
	Access         ProjectAccess
	// OnLimitsOverride : limites posées à la main par l'équipe (exception au
	// plan de l'organisation, retenue par la facturation). nil : rien.
	OnLimitsOverride func(ctx context.Context, projectID string, limits typeiam.UsageDTO, by string) error
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
	out, err := h.ProjectService.ListProjects(c.Request.Context(), held.Organizations, held.Projects, held.Excluded)
	if err != nil {
		return nil, err
	}
	// Où l'appelant peut créer un projet, avec le plafond de chaque
	// organisation. Vide : la création ira dans son organisation personnelle.
	canCreate, err := h.Access.Holdings(c.Request.Context(), p, iam.ProjectsCreate)
	if err != nil {
		return nil, err
	}
	if out.Organizations, err = h.ProjectService.OrganizationQuotas(c.Request.Context(), canCreate.Organizations); err != nil {
		return nil, err
	}
	return out, nil
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
	// Organisation choisie : il faut y avoir le droit de créer un projet.
	// Sans choix, l'organisation personnelle (dont l'appelant est owner).
	if in.OrganizationID != "" {
		org, err := uuid.Parse(in.OrganizationID)
		if err != nil {
			return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
		}
		p, ok := security.PrincipalFromContext(c)
		if !ok || h.Access == nil {
			return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied")
		}
		dec, err := h.Access.Check(c.Request.Context(), p, iam.ProjectsCreate, iam.OrganizationName(org))
		if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
			return nil, err
		}
		if err != nil || !dec.Allowed {
			return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied: resourcemanager.projects.create on this organization")
		}
	}
	res, err := h.ProjectService.CreateProject(c.Request.Context(), *in, owner.String())
	if err != nil {
		return nil, err
	}
	// Journal : création rattachée à l'organisation (son journal) et au
	// nouveau projet (le sien).
	if pid, perr := uuid.Parse(res.ProjectID); perr == nil {
		if d, gerr := h.ProjectService.GetProject(c.Request.Context(), res.ProjectID); gerr == nil {
			if org, oerr := uuid.Parse(d.OrganizationID); oerr == nil {
				security.RecordAudit(c, iam.ProjectsCreate, iam.OrganizationName(org), pid)
			}
		}
	}
	return res, nil
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

// SetProjectLimitsRequest : nouvelles limites d'un projet ; un champ vide
// garde la valeur actuelle.
type SetProjectLimitsRequest struct {
	ProjectID string `path:"id" description:"ID du projet"`
	typeiam.ProjectLimits
}

// SetProjectLimits : réservé au staff de la plateforme (garde de la route).
// Les limites ainsi posées sont une exception au plan : la facturation la
// retient pour que le rapprochement des plans ne l'écrase pas.
func (h *ProjectHandler) SetProjectLimits(c *gin.Context, in *SetProjectLimitsRequest) (*typeiam.ProjectLimitsResponse, error) {
	res, err := h.ProjectService.SetLimits(c.Request.Context(), in.ProjectID, in.ProjectLimits)
	if err != nil || h.OnLimitsOverride == nil {
		return res, err
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	if err := h.OnLimitsOverride(c.Request.Context(), in.ProjectID, res.Limits, by); err != nil {
		return nil, err
	}
	return res, nil
}

// GetProject : un projet et ses plafonds.
func (h *ProjectHandler) GetProject(c *gin.Context, in *GetProjectStatusRequest) (*typeiam.ProjectDetail, error) {
	return h.ProjectService.GetProject(c.Request.Context(), in.ProjectID)
}

// UpdateProject : renommer, changer la description.
func (h *ProjectHandler) UpdateProject(c *gin.Context, in *typeiam.UpdateProjectRequest) (*typeiam.ProjectDetail, error) {
	return h.ProjectService.UpdateProject(c.Request.Context(), *in)
}

// SetOrganizationLimitsRequest : nombre maximal de projets d'une organisation.
type SetOrganizationLimitsRequest struct {
	OrganizationID string `path:"id" binding:"required" description:"ID de l'organisation"`
	MaxProjects    int    `json:"max_projects" binding:"required,min=1,max=1000" description:"Nombre maximal de projets"`
}

// SetOrganizationLimits : réservé au staff de la plateforme (garde de la route).
func (h *ProjectHandler) SetOrganizationLimits(c *gin.Context, in *SetOrganizationLimitsRequest) (*typeiam.OrganizationQuota, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return h.ProjectService.SetMaxProjects(c.Request.Context(), org, in.MaxProjects)
}
