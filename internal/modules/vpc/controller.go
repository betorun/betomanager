// Package vpc : réseau privé (VPC) d'un projet étendu à ses VPS (ADR 0019).
package vpc

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/service"
)

// ProjectPath : projet visé.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

// VPSPath : VPS d'une organisation.
type VPSPath struct {
	OrganizationID string `path:"id" binding:"required" description:"ID de l'organisation"`
	VPSID          string `path:"vpsID" binding:"required" description:"ID du VPS"`
}

type Controller struct{ Service *service.Service }

// Get : réseau privé (VPC) du projet et ses VPS.
func (h *Controller) Get(c *gin.Context, in *ProjectPath) (*service.View, error) {
	id, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}
	return h.Service.Get(c.Request.Context(), id)
}

func (in *VPSPath) ids() (uuid.UUID, uuid.UUID, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "organization not found")
	}
	id, err := uuid.Parse(in.VPSID)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "VPS not found")
	}
	return org, id, nil
}

// Attach : rattacher un VPS au réseau privé de son projet.
func (h *Controller) Attach(c *gin.Context, in *VPSPath) (*service.AttachResult, error) {
	org, id, err := in.ids()
	if err != nil {
		return nil, err
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.Attach(c.Request.Context(), org, id, by)
}

// Detach : détacher un VPS du réseau privé.
func (h *Controller) Detach(c *gin.Context, in *VPSPath) error {
	org, id, err := in.ids()
	if err != nil {
		return err
	}
	return h.Service.Detach(c.Request.Context(), org, id)
}

// Enroll : inscription envoyée par le VPS lui-même (jeton à usage unique).
func (h *Controller) Enroll(c *gin.Context, in *service.EnrollInput) (*service.EnrollOutput, error) {
	return h.Service.Enroll(c.Request.Context(), *in)
}
