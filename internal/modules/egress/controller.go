// Package egress : IP de sortie dédiée par projet (ADR 0014).
package egress

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
	"github.com/thekrauss/kubemanager/internal/modules/egress/service"
)

type Controller struct{ Service *service.Service }

// ProjectPath : projet visé.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

// ClusterPath : cluster visé (back-office).
type ClusterPath struct {
	ClusterID string `path:"clusterID" binding:"required" description:"Identifiant du cluster"`
}

// DeclareInput : nouvelle IP de la réserve d'un cluster.
type DeclareInput struct {
	ClusterID string `path:"clusterID" binding:"required"`
	service.DeclareInput
}

// AddressPath : une IP de la réserve.
type AddressPath struct {
	ClusterID string `path:"clusterID" binding:"required"`
	AddressID string `path:"addressID" binding:"required" description:"Identifiant de l'adresse dans la réserve"`
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

func project(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}
	return id, nil
}

// Get : IP dédiée du projet, ou si l'on peut en demander une.
func (h *Controller) Get(c *gin.Context, in *ProjectPath) (*domain.ProjectEgress, error) {
	id, err := project(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.Get(c.Request.Context(), id)
}

// Request : une IP dédiée pour le projet.
func (h *Controller) Request(c *gin.Context, in *ProjectPath) (*domain.ProjectEgress, error) {
	id, err := project(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.Request(c.Request.Context(), id, principal(c))
}

// Release : l'IP dédiée rendue (le trafic ressort par une adresse partagée).
func (h *Controller) Release(c *gin.Context, in *ProjectPath) error {
	id, err := project(in.ProjectID)
	if err != nil {
		return err
	}
	return h.Service.Release(c.Request.Context(), id)
}

// AdminList : réserve d'un cluster.
func (h *Controller) AdminList(c *gin.Context, in *ClusterPath) ([]domain.AdminAddress, error) {
	return h.Service.List(c.Request.Context(), in.ClusterID)
}

// AdminDeclare : IP ajoutée à la réserve.
func (h *Controller) AdminDeclare(c *gin.Context, in *DeclareInput) (*domain.AdminAddress, error) {
	return h.Service.Declare(c.Request.Context(), in.ClusterID, in.DeclareInput, principal(c))
}

// AdminRemove : IP libre retirée.
func (h *Controller) AdminRemove(c *gin.Context, in *AddressPath) error {
	id, err := uuid.Parse(in.AddressID)
	if err != nil {
		return domain.ErrAddressNotFound
	}
	return h.Service.Remove(c.Request.Context(), in.ClusterID, id)
}
