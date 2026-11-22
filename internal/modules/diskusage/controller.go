package diskusage

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
	"github.com/thekrauss/kubemanager/internal/modules/diskusage/service"
)

// Controller : GET /projects/:id/disk-usage.
type Controller struct{ Service *service.Service }

// List : dernière mesure de chaque disque du projet (vide avant le premier passage).
func (c *Controller) List(ctx *gin.Context, in *ProjectPath) (*domain.UsageList, error) {
	id, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	return c.Service.List(ctx.Request.Context(), id)
}

// ProjectPath : projet visé.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}
