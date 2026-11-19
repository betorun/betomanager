package diskusage

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
)

// Controller : GET /projects/:id/disk-usage.
type Controller struct{ Service *Service }

// List : dernière mesure de chaque disque du projet (vide avant le premier passage).
func (c *Controller) List(ctx *gin.Context, in *ProjectPath) (*UsageList, error) {
	id, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	return c.Service.List(ctx.Request.Context(), id)
}
