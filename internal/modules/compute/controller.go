// Package compute : VPS revendus, routes HTTP (docs/modules/compute.md). La
// logique est dans service, l'exécution dans workflows (mode compute).
package compute

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute/workflows"
)

// Controller : offres (clients), réglage des offres et diagnostic (équipe).
type Controller struct {
	Service *service.Service
	DB      *gorm.DB
}

type OrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

type OffersResponse struct {
	Offers []service.OfferView `json:"offers"`
}

// Offers : offres en vente, prix mensuel dans la monnaie de l'organisation.
func (h *Controller) Offers(c *gin.Context, in *OrgPath) (*OffersResponse, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	list, err := h.Service.Offers(c.Request.Context(), org)
	if err != nil {
		return nil, err
	}
	return &OffersResponse{Offers: list}, nil
}

type VPSListResponse struct {
	VPS []domain.VPS `json:"vps"`
}

// List : VPS de l'organisation (hors résiliés).
func (h *Controller) List(c *gin.Context, in *OrgPath) (*VPSListResponse, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	list, err := h.Service.List(c.Request.Context(), org)
	if err != nil {
		return nil, err
	}
	return &VPSListResponse{VPS: list}, nil
}

type VPSPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	VPSID          string `path:"vpsID" description:"Identifiant du VPS"`
}

func (h *Controller) Get(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	id, err := uuid.Parse(in.VPSID)
	if err != nil {
		return nil, domain.ErrVPSNotFound
	}
	return h.Service.Get(c.Request.Context(), org, id)
}

// Order : premier mois débité et demande de création déposés ensemble ;
// le VPS passe de « pending » à « creating » puis « running ».
func (h *Controller) Order(c *gin.Context, in *service.OrderRequest) (*domain.VPS, error) {
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.Order(c.Request.Context(), *in, by)
}

type AdminOffersResponse struct {
	Offers []domain.Offer `json:"offers"`
}

func (h *Controller) AdminOffers(c *gin.Context) (*AdminOffersResponse, error) {
	list, err := h.Service.AdminOffers(c.Request.Context())
	if err != nil {
		return nil, err
	}
	return &AdminOffersResponse{Offers: list}, nil
}

func (h *Controller) SaveOffer(c *gin.Context, in *service.OfferUpdate) (*domain.Offer, error) {
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.SaveOffer(c.Request.Context(), *in, by)
}

type PingResponse struct {
	RequestID string `json:"request_id"`
}

// Ping : une demande compute.ping traverse la chaîne (outbox → relais →
// Temporal → mode compute → base) ; suivre son état par GET /admin/compute/requests/:id.
func (h *Controller) Ping(c *gin.Context) (*PingResponse, error) {
	return h.enqueue(c, workflows.KindPing)
}

// Reconcile : une passe de réconciliation tout de suite (en plus de la passe
// périodique du mode compute).
func (h *Controller) Reconcile(c *gin.Context) (*PingResponse, error) {
	return h.enqueue(c, workflows.KindReconcile)
}

// enqueue : demande de l'équipe, sans organisation.
func (h *Controller) enqueue(c *gin.Context, kind string) (*PingResponse, error) {
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	r := &domain.Request{OrganizationID: uuid.Nil, Kind: kind, CreatedBy: by}
	if err := h.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		return h.Service.Store.EnqueueTx(tx, r)
	}); err != nil {
		return nil, err
	}
	return &PingResponse{RequestID: r.ID.String()}, nil
}

// Fleet : VPS de tous les clients, relevé du fournisseur, écarts, passes.
func (h *Controller) Fleet(c *gin.Context) (*service.Fleet, error) {
	return h.Service.Fleet(c.Request.Context())
}

type RequestPath struct {
	RequestID string `path:"requestID" description:"Identifiant de la demande"`
}

// ErrRequestNotFound : demande inconnue.
var ErrRequestNotFound = errors.New("compute request not found")

func (h *Controller) Request(c *gin.Context, in *RequestPath) (*domain.Request, error) {
	id, err := uuid.Parse(strings.TrimSpace(in.RequestID))
	if err != nil {
		return nil, ErrRequestNotFound
	}
	r, err := h.Service.Store.Request(c.Request.Context(), id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrRequestNotFound
	}
	return r, nil
}
