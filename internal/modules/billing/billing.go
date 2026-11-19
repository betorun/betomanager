// Package billing : API de la facturation (docs/adr/0012-facturation.md).
// Phase 2 : coût estimé d'une organisation et grille de prix ; rien n'est
// débité.
package billing

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/billing/xykpay"
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
	return h.Service.UpdateCatalog(c.Request.Context(), *in, principal(c))
}

func parseOrg(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return id, nil
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

// ── Wallet ──────────────────────────────────────────────────────────────

// Wallet : solde de l'organisation.
func (h *Controller) Wallet(c *gin.Context, in *domain.OrgPath) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Wallet(c.Request.Context(), org)
}

// Transactions : historique des mouvements.
func (h *Controller) Transactions(c *gin.Context, in *domain.TransactionsRequest) (*domain.TransactionsResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Transactions(c.Request.Context(), org, in.Before, in.Limit)
}

// CreateTopup : recharge par mobile money.
func (h *Controller) CreateTopup(c *gin.Context, in *domain.TopupRequest) (*domain.TopupView, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.CreateTopup(c.Request.Context(), org, *in, principal(c))
}

// Topups : recharges récentes.
func (h *Controller) Topups(c *gin.Context, in *domain.OrgPath) (*domain.TopupsResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Topups(c.Request.Context(), org)
}

// Topup : une recharge (relue chez XYKPay si elle attend).
func (h *Controller) Topup(c *gin.Context, in *domain.TopupPath) (*domain.TopupView, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.TopupID)
	if err != nil {
		return nil, domain.ErrTopupNotFound
	}
	return h.Service.Topup(c.Request.Context(), org, id)
}

// Webhook : signal de XYKPay (corps brut, signature vérifiée).
func (h *Controller) Webhook(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "INVALID_INPUT", "message": "unreadable body"})
		return
	}
	switch err := h.Service.HandleWebhook(c.Request.Context(), c.GetHeader(xykpay.SignatureHeader), body); {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, domain.ErrBadWebhook):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "INVALID_SIGNATURE", "message": "invalid signature"})
	case errors.Is(err, domain.ErrPaymentsDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": "PAYMENTS_DISABLED", "message": "payments are not configured"})
	default:
		// XYKPay relivrera ; le worker relit aussi les recharges en attente.
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": "UNAVAILABLE", "message": "try again later"})
	}
}

// ── Administration ──────────────────────────────────────────────────────

// AdminOrgs : recherche d'organisations.
func (h *Controller) AdminOrgs(c *gin.Context, in *domain.AdminOrgsRequest) (*domain.OrgsResponse, error) {
	return h.Service.Orgs(c.Request.Context(), in.Query, in.Limit)
}

// AdminOrg : fiche d'une organisation.
func (h *Controller) AdminOrg(c *gin.Context, in *domain.AdminOrgPath) (*domain.OrgDetail, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Org(c.Request.Context(), org)
}

// AdminCredit : offrir un crédit.
func (h *Controller) AdminCredit(c *gin.Context, in *domain.CreditRequest) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.GrantCredit(c.Request.Context(), org, *in, principal(c))
}

// AdminAdjust : corriger l'argent d'un client.
func (h *Controller) AdminAdjust(c *gin.Context, in *domain.AdjustmentRequest) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Adjust(c.Request.Context(), org, *in, principal(c))
}

// AdminCharging : activer ou suspendre la facturation réelle.
func (h *Controller) AdminCharging(c *gin.Context, in *domain.ChargingRequest) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.SetCharging(c.Request.Context(), org, in.Enabled, principal(c))
}

// AdminRevenue : recettes.
func (h *Controller) AdminRevenue(c *gin.Context, in *domain.RevenueRequest) (*domain.RevenueResponse, error) {
	return h.Service.Revenue(c.Request.Context(), in.Days)
}

// ── Phase 4 ─────────────────────────────────────────────────────────────

// Profile : informations portées sur les factures.
func (h *Controller) Profile(c *gin.Context, in *domain.OrgPath) (*domain.Profile, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Profile(c.Request.Context(), org)
}

// SetProfile : modifie les informations de facturation.
func (h *Controller) SetProfile(c *gin.Context, in *domain.ProfileRequest) (*domain.Profile, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.SetProfile(c.Request.Context(), org, *in)
}

// Documents : factures et reçus.
func (h *Controller) Documents(c *gin.Context, in *domain.OrgPath) (*domain.DocumentsResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Documents(c.Request.Context(), org)
}

// DocumentPDF : le PDF archivé (application/pdf).
func (h *Controller) DocumentPDF(c *gin.Context, in *domain.DocumentPath) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "INVALID_INPUT", "message": "invalid organization id"})
		return
	}
	id, err := uuid.Parse(in.DocumentID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "document not found"})
		return
	}
	d, err := h.Service.DocumentPDF(c.Request.Context(), org, id)
	if errors.Is(err, domain.ErrDocumentNotFound) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "document not found"})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": "UNAVAILABLE", "message": "try again later"})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+d.Number+`.pdf"`)
	c.Header("Cache-Control", "private, max-age=3600")
	c.Data(http.StatusOK, "application/pdf", d.PDF)
}

func parseProject(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	return id, nil
}

// Budget : budget mensuel d'un projet.
func (h *Controller) Budget(c *gin.Context, in *domain.ProjectPath) (*domain.Budget, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.Budget(c.Request.Context(), project)
}

// SetBudget : fixe ou retire le budget d'un projet.
func (h *Controller) SetBudget(c *gin.Context, in *domain.BudgetRequest) (*domain.Budget, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.SetBudget(c.Request.Context(), project, *in, principal(c))
}

// ── Plans ───────────────────────────────────────────────────────────────

// PublicPlans : plans proposés (page Tarifs).
func (h *Controller) PublicPlans(c *gin.Context, in *domain.PlansRequest) (*domain.PlansResponse, error) {
	return h.Service.PublicPlans(c.Request.Context(), in.Currency)
}

// OrgPlan : plan de l'organisation et plans proposés.
func (h *Controller) OrgPlan(c *gin.Context, in *domain.OrgPath) (*domain.OrgPlan, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.OrgPlan(c.Request.Context(), org)
}

// ChangePlan : le client change de plan.
func (h *Controller) ChangePlan(c *gin.Context, in *domain.ChangePlanRequest) (*domain.OrgPlan, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.ChangePlan(c.Request.Context(), org, in.Plan, principal(c), false)
}

// AdminChangePlan : l'équipe attribue un plan (y compris non public, sans
// facturation active).
func (h *Controller) AdminChangePlan(c *gin.Context, in *domain.ChangePlanRequest) (*domain.OrgPlan, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.ChangePlan(c.Request.Context(), org, in.Plan, principal(c), true)
}

// AdminPlans : tous les plans.
func (h *Controller) AdminPlans(c *gin.Context) (*domain.AdminPlansResponse, error) {
	return h.Service.AdminPlans(c.Request.Context())
}

// AdminSavePlan : crée ou modifie un plan ; ses limites suivent aussitôt.
func (h *Controller) AdminSavePlan(c *gin.Context, in *domain.PlanUpdate) (*domain.SavePlanResponse, error) {
	return h.Service.SavePlan(c.Request.Context(), *in, principal(c))
}

// AdminClearOverride : le projet revient aux limites de son plan.
func (h *Controller) AdminClearOverride(c *gin.Context, in *domain.ProjectLimitsPath) (*domain.SavePlanResponse, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	missed, err := h.Service.ClearOverride(c.Request.Context(), project)
	if err != nil {
		return nil, err
	}
	return &domain.SavePlanResponse{NotApplied: missed}, nil
}

// ── Postpayé ────────────────────────────────────────────────────────────

// AdminPostpaid : passer une organisation en postpayé, ou au prépayé.
func (h *Controller) AdminPostpaid(c *gin.Context, in *domain.PostpaidRequest) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.SetPostpaid(c.Request.Context(), org, *in, principal(c))
}

// AdminPayment : virement reçu.
func (h *Controller) AdminPayment(c *gin.Context, in *domain.PaymentRequest) (*domain.Wallet, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.RecordPayment(c.Request.Context(), org, *in, principal(c))
}
