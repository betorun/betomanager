// Package billing : API de la facturation (docs/adr/0012-facturation.md).
// Phase 2 : coût estimé d'une organisation et grille de prix ; rien n'est
// débité.
package billing

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/service"
)

// Controller : routes de la facturation.
type Controller struct {
	Service *service.Service
}

// Estimate : coût estimé d'une organisation pour un mois.
func (h *Controller) Estimate(c *gin.Context, in *domain.EstimateRequest) (*domain.Estimate, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return h.Service.Estimate(c.Request.Context(), org, in.Month)
}

// Catalog : grille de prix d'une monnaie.
func (h *Controller) Catalog(c *gin.Context, in *domain.CatalogRequest) (*domain.CatalogView, error) {
	return h.Service.Catalog(c.Request.Context(), in.Currency)
}

// Catalogs : grilles en vigueur (équipe de la plateforme).
func (h *Controller) Catalogs(c *gin.Context) (*domain.CatalogsResponse, error) {
	return h.Service.Catalogs(c.Request.Context())
}

// UpdateCatalog : nouvelle version de la grille d'une monnaie (équipe de la
// plateforme).
func (h *Controller) UpdateCatalog(c *gin.Context, in *domain.CatalogUpdate) (*domain.CatalogView, error) {
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.UpdateCatalog(c.Request.Context(), *in, by)
}
