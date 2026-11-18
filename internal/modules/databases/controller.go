package databases

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Authorizer : décision IAM (iam.Engine).
type Authorizer interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

// Controller : routes /projects/:id/databases.
type Controller struct {
	Service *Service
	// IAM : second contrôle de l'attache (variables du conteneur).
	IAM Authorizer
}

func NewController(s *Service) *Controller { return &Controller{Service: s} }

// apiError : erreurs du service → statut HTTP et message stable.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidStorage), errors.Is(err, domain.ErrInvalidVariable):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, "database not found")
	case errors.Is(err, domain.ErrWorkloadNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrNotReady):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrNameTaken):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case strings.HasPrefix(err.Error(), "at most "), strings.HasPrefix(err.Error(), "project is not ready"):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	}
	return err
}

func ids(project, db string) (uuid.UUID, uuid.UUID, error) {
	p, err := uuid.Parse(project)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	if db == "" {
		return p, uuid.Nil, nil
	}
	d, err := uuid.Parse(db)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "database not found")
	}
	return p, d, nil
}

func (h *Controller) List(c *gin.Context, in *domain.ProjectPath) ([]domain.DatabaseView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	out, err := h.Service.List(c.Request.Context(), p)
	return out, apiError(err)
}

func (h *Controller) Get(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Get(c.Request.Context(), p, d)
	return out, apiError(err)
}

func (h *Controller) Create(c *gin.Context, in *domain.CreateDatabaseRequest) (*domain.DatabaseView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	creator := ""
	if principal, ok := security.PrincipalFromContext(c); ok {
		creator = string(principal)
	}
	out, err := h.Service.Create(c.Request.Context(), p, *in, creator)
	return out, apiError(err)
}

func (h *Controller) Delete(c *gin.Context, in *domain.DatabasePath) error {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return err
	}
	return apiError(h.Service.Delete(c.Request.Context(), p, d))
}

// Attach : la route a vérifié databases.databases.connect sur la base ; il
// faut AUSSI pouvoir modifier les variables du conteneur visé (sinon un
// développeur d'un projet pourrait écrire dans un conteneur qu'il ne gère pas).
func (h *Controller) Attach(c *gin.Context, in *domain.AttachRequest) (*domain.AttachResponse, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	w, err := uuid.Parse(strings.TrimSpace(in.WorkloadID))
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrWorkloadNotFound.Error())
	}
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	res := iam.ProjectName(p).Child("workloads", w)
	dec, err := h.IAM.Check(c.Request.Context(), principal, iam.WorkloadVariablesUpdate, res)
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return nil, err
	}
	if err != nil || !dec.Allowed {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied: workloads.variables.update on this container")
	}
	out, err := h.Service.Attach(c.Request.Context(), p, d, w, strings.TrimSpace(in.Variable))
	return out, apiError(err)
}
