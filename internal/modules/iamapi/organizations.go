package iamapi

import (
	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/core/country"
	orgDomain "github.com/thekrauss/kubemanager/internal/modules/organizations/domain"
	orgRepo "github.com/thekrauss/kubemanager/internal/modules/organizations/repository"
)

// ── Organisation : vue et réglages ───────────────────────────────────────

// UpdateOrganizationRequest : seuls les champs présents changent.
type UpdateOrganizationRequest struct {
	OrgID   string  `path:"id" binding:"required"`
	Country *string `json:"country,omitempty" description:"Pays du client (code ISO 3166-1 à deux lettres, ex. GA ; vide : inconnu). Il désigne la région suggérée à la création d'un projet."`
}

// GetOrganization : nom, type et pays de l'organisation.
func (h *Controller) GetOrganization(c *gin.Context, in *OrgPath) (*orgDomain.Organization, error) {
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return orgRepo.Get(c.Request.Context(), h.DB, id[0])
}

// UpdateOrganization : pays de l'organisation.
func (h *Controller) UpdateOrganization(c *gin.Context, in *UpdateOrganizationRequest) (*orgDomain.Organization, error) {
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	ctx := c.Request.Context()
	if in.Country != nil {
		code, err := country.Parse(*in.Country)
		if err != nil {
			return nil, err
		}
		if err := orgRepo.SetCountry(ctx, h.DB, id[0], code); err != nil {
			return nil, err
		}
		audit.Annotate(ctx, "country", code)
	}
	return orgRepo.Get(ctx, h.DB, id[0])
}
