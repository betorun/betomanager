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

func (h *Controller) act(c *gin.Context, in *VPSPath, action string, re *service.ReinstallRequest) (*domain.VPS, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	id, err := uuid.Parse(in.VPSID)
	if err != nil {
		return nil, domain.ErrVPSNotFound
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.Act(c.Request.Context(), org, id, action, re, by)
}

// Start / Stop / Reboot : une opération à la fois ; suivre l'état du VPS
// (champ operation, puis state) ou le canal temps réel du VPS.
func (h *Controller) Start(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	return h.act(c, in, service.ActionStart, nil)
}

func (h *Controller) Stop(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	return h.act(c, in, service.ActionStop, nil)
}

func (h *Controller) Reboot(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	return h.act(c, in, service.ActionReboot, nil)
}

type ReinstallInput struct {
	VPSPath
	service.ReinstallRequest
}

// Reinstall : nouveau système avec la clé donnée ; le disque est effacé.
func (h *Controller) Reinstall(c *gin.Context, in *ReinstallInput) (*domain.VPS, error) {
	return h.act(c, &in.VPSPath, service.ActionReinstall, &in.ReinstallRequest)
}

func (h *Controller) ids(c *gin.Context, in *VPSPath) (uuid.UUID, uuid.UUID, string, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	id, err := uuid.Parse(in.VPSID)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", domain.ErrVPSNotFound
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return org, id, by, nil
}

// Backups : dernier relevé des sauvegardes et prix de l'option snapshot.
func (h *Controller) Backups(c *gin.Context, in *VPSPath) (*service.BackupsView, error) {
	org, id, _, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.Backups(c.Request.Context(), org, id)
}

type RestoreInput struct {
	VPSPath
	service.RestoreRequest
}

// Restore : full remplace le disque ; mount attache la sauvegarde en disque supplémentaire.
func (h *Controller) Restore(c *gin.Context, in *RestoreInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.Restore(c.Request.Context(), org, id, in.RestoreRequest, by)
}

type UnmountInput struct {
	VPSPath
	service.UnmountRequest
}

func (h *Controller) Unmount(c *gin.Context, in *UnmountInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.Unmount(c.Request.Context(), org, id, in.UnmountRequest, by)
}

type SnapshotInput struct {
	VPSPath
	service.SnapshotRequest
}

func (h *Controller) CreateSnapshot(c *gin.Context, in *SnapshotInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.CreateSnapshot(c.Request.Context(), org, id, in.SnapshotRequest, by)
}

func (h *Controller) RevertSnapshot(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.RevertSnapshot(c.Request.Context(), org, id, by)
}

func (h *Controller) DeleteSnapshot(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.DeleteSnapshot(c.Request.Context(), org, id, by)
}

type SubscribeInput struct {
	VPSPath
	service.SubscribeRequest
}

func (h *Controller) SubscribeSnapshot(c *gin.Context, in *SubscribeInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.SubscribeSnapshot(c.Request.Context(), org, id, in.SubscribeRequest, by)
}

func (h *Controller) UnsubscribeSnapshot(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.UnsubscribeSnapshot(c.Request.Context(), org, id, by)
}

// EnterRescue / ExitRescue : démarrage sur le système de secours, retour au disque.
func (h *Controller) EnterRescue(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.EnterRescue(c.Request.Context(), org, id, by)
}

func (h *Controller) ExitRescue(c *gin.Context, in *VPSPath) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	return h.Service.ExitRescue(c.Request.Context(), org, id, by)
}

// RevealRescue : identifiants du mode secours, une seule fois.
func (h *Controller) RevealRescue(c *gin.Context, in *VPSPath) (*service.RescueReveal, error) {
	org, id, _, err := h.ids(c, in)
	if err != nil {
		return nil, err
	}
	c.Header("Cache-Control", "no-store")
	return h.Service.RevealRescue(c.Request.Context(), org, id)
}

type ReverseInput struct {
	VPSPath
	service.ReverseRequest
}

// SetReverse : DNS inverse ; le nom doit déjà pointer vers l'adresse.
func (h *Controller) SetReverse(c *gin.Context, in *ReverseInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.SetReverse(c.Request.Context(), org, id, in.ReverseRequest, by)
}

type TerminateInput struct {
	VPSPath
	service.TerminateRequest
}

// Terminate : résiliation (coupé tout de suite, plus renouvelé, pas de remboursement).
func (h *Controller) Terminate(c *gin.Context, in *TerminateInput) (*domain.VPS, error) {
	org, id, by, err := h.ids(c, &in.VPSPath)
	if err != nil {
		return nil, err
	}
	return h.Service.Terminate(c.Request.Context(), org, id, in.TerminateRequest, by)
}

type AdminVPSPath struct {
	VPSID string `path:"vpsID" description:"Identifiant du VPS"`
}

func adminVPS(in *AdminVPSPath) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(in.VPSID))
	if err != nil {
		return uuid.Nil, domain.ErrVPSNotFound
	}
	return id, nil
}

// UnblockSpam : l'équipe lève le blocage anti-spam (après vérification).
func (h *Controller) UnblockSpam(c *gin.Context, in *AdminVPSPath) (*domain.VPS, error) {
	id, err := adminVPS(in)
	if err != nil {
		return nil, err
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	return h.Service.UnblockSpam(c.Request.Context(), id, by)
}

// SetSMTP : port 25 sortant ouvert ou fermé (prochaine installation).
func (h *Controller) SetSMTP(c *gin.Context, in *service.SMTPRequest) (*domain.VPS, error) {
	id, err := adminVPS(&AdminVPSPath{VPSID: in.VPSID})
	if err != nil {
		return nil, err
	}
	return h.Service.SetSMTP(c.Request.Context(), id, in.Allowed)
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

// ── Lot 8 : quotas, anti-fraude, abus, e-mails du fournisseur (équipe) ──

func (h *Controller) Limits(c *gin.Context) (*domain.Limits, error) {
	return h.Service.Limits(c.Request.Context())
}

func (h *Controller) SaveLimits(c *gin.Context, in *service.LimitsUpdate) (*domain.Limits, error) {
	return h.Service.SaveLimits(c.Request.Context(), *in, principal(c))
}

func (h *Controller) OrgLimits(c *gin.Context, in *OrgPath) (*service.OrgLimitsView, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return h.Service.OrgLimitsView(c.Request.Context(), org)
}

func (h *Controller) SetOrgLimits(c *gin.Context, in *service.OrgLimitsUpdate) (*service.OrgLimitsView, error) {
	org, err := uuid.Parse(in.OrganizationID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return h.Service.SetOrgLimits(c.Request.Context(), org, *in, principal(c))
}

func (h *Controller) SuspendForAbuse(c *gin.Context, in *service.AbuseRequest) (*domain.VPS, error) {
	id, err := adminVPS(&AdminVPSPath{VPSID: in.VPSID})
	if err != nil {
		return nil, err
	}
	return h.Service.SuspendForAbuse(c.Request.Context(), id, in.Note, principal(c))
}

func (h *Controller) ResumeFromAbuse(c *gin.Context, in *AdminVPSPath) (*domain.VPS, error) {
	id, err := adminVPS(in)
	if err != nil {
		return nil, err
	}
	return h.Service.ResumeFromAbuse(c.Request.Context(), id, principal(c))
}

func (h *Controller) AdminTerminate(c *gin.Context, in *AdminVPSPath) (*domain.VPS, error) {
	id, err := adminVPS(in)
	if err != nil {
		return nil, err
	}
	return h.Service.AdminTerminate(c.Request.Context(), id, principal(c))
}

// AttachInput : rattacher un VPS livré à son identifiant chez le fournisseur.
type AttachInput struct {
	VPSID      string `path:"vpsID" json:"-"`
	ProviderID string `json:"provider_id" binding:"required" description:"Identifiant du VPS chez le fournisseur (orphelin du relevé)"`
}

func (h *Controller) Attach(c *gin.Context, in *AttachInput) (*domain.VPS, error) {
	id, err := adminVPS(&AdminVPSPath{VPSID: in.VPSID})
	if err != nil {
		return nil, err
	}
	return h.Service.AttachDelivered(c.Request.Context(), id, in.ProviderID)
}

type ProviderMailsInput struct {
	All bool `query:"all" description:"Inclure les e-mails déjà traités"`
}

type ProviderMailsResponse struct {
	Mails []domain.ProviderMail `json:"mails"`
}

func (h *Controller) ProviderMails(c *gin.Context, in *ProviderMailsInput) (*ProviderMailsResponse, error) {
	list, err := h.Service.ProviderMails(c.Request.Context(), !in.All)
	if err != nil {
		return nil, err
	}
	return &ProviderMailsResponse{Mails: list}, nil
}

type MailPath struct {
	MailID string `path:"mailID" description:"Identifiant de l'e-mail"`
}

func (h *Controller) HandleProviderMail(c *gin.Context, in *MailPath) error {
	id, err := uuid.Parse(in.MailID)
	if err != nil {
		return domain.ErrVPSNotFound
	}
	return h.Service.HandleProviderMail(c.Request.Context(), id, principal(c))
}

// MailboxCheck : diagnostic de la boîte technique (demande compute.mailbox).
func (h *Controller) MailboxCheck(c *gin.Context) (*PingResponse, error) {
	return h.enqueue(c, workflows.KindMailbox)
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
	Offers []service.AdminOffer `json:"offers"`
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
