// Package domainsales : API de la revente de noms de domaine (ADR 0012,
// phase 5) : recherche, achat payé par le solde, renouvellement, branchement
// sur un conteneur.
package domainsales

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/service"
)

// Controller : routes des domaines.
type Controller struct {
	Service *service.Service
}

func parseOrg(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return id, nil
}

func parseOrder(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.ErrOrderNotFound
	}
	return id, nil
}

// Search : disponibilité et prix.
func (h *Controller) Search(c *gin.Context, in *domain.SearchRequest) (*domain.Quote, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Search(c.Request.Context(), org, in.Domain)
}

// List : domaines de l'organisation.
func (h *Controller) List(c *gin.Context, in *domain.OrgPath) (*domain.OrdersResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.List(c.Request.Context(), org)
}

// Buy : acheter un domaine.
func (h *Controller) Buy(c *gin.Context, in *domain.BuyRequest) (*domain.OrderView, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.Buy(c.Request.Context(), org, *in, by)
}

// SetAutoRenew : renouvellement automatique.
func (h *Controller) SetAutoRenew(c *gin.Context, in *domain.AutoRenewRequest) (*domain.OrderView, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	id, err := parseOrder(in.OrderID)
	if err != nil {
		return nil, err
	}
	return h.Service.SetAutoRenew(c.Request.Context(), org, id, in.Enabled)
}

// Attach : brancher le domaine sur un conteneur.
func (h *Controller) Attach(c *gin.Context, in *domain.AttachRequest) (*domain.AttachResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	id, err := parseOrder(in.OrderID)
	if err != nil {
		return nil, err
	}
	return h.Service.Attach(c.Request.Context(), org, id, *in)
}
