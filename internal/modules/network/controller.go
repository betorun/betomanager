// Package network : réseau privé d'un projet (console, CLI, Copilot).
package network

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/modules/network/domain"
	"github.com/thekrauss/kubemanager/internal/modules/network/service"
)

// ProjectPath : projet visé.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

type Controller struct{ Service *service.Service }

// Get : membres, liens déclarés et règles du réseau du projet.
func (h *Controller) Get(c *gin.Context, in *ProjectPath) (*domain.ProjectNetwork, error) {
	id, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}
	return h.Service.Get(c.Request.Context(), id)
}
