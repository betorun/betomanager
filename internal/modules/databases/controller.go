package databases

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Controller : routes /projects/:id/databases.
type Controller struct {
	Service *Service
}

func NewController(s *Service) *Controller { return &Controller{Service: s} }

// apiError : erreurs du service → statut HTTP et message stable.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidStorage):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, "database not found")
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
