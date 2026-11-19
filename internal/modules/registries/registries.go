// Package registries : API des accès d'un projet à ses registres d'images
// externes (docs/adr/0011-registres-externes.md).
package registries

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	"github.com/thekrauss/kubemanager/internal/modules/registries/service"
)

// Controller : routes des registres externes.
type Controller struct {
	Service *service.Service
}

func parseProject(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	return id, nil
}

// List : accès du projet (jamais le jeton).
func (h *Controller) List(c *gin.Context, in *domain.ProjectPath) (*domain.ListResponse, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.List(c.Request.Context(), project)
}

// Put : créer ou remplacer l'accès à un registre.
func (h *Controller) Put(c *gin.Context, in *domain.PutRequest) (*domain.View, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	var by *uuid.UUID
	if uid, ok := security.InternalUserID(c); ok {
		by = &uid
	}
	return h.Service.Put(c.Request.Context(), project, *in, by)
}

// Delete : retirer l'accès.
func (h *Controller) Delete(c *gin.Context, in *domain.HostPath) error {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return err
	}
	return h.Service.Delete(c.Request.Context(), project, in.Host)
}

// Test : vérifier l'accès auprès du registre.
func (h *Controller) Test(c *gin.Context, in *domain.HostPath) (*domain.TestResponse, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.Test(c.Request.Context(), project, in.Host)
}
