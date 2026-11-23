// Package legal : conditions générales de vente et politique d'usage
// acceptable, routes HTTP (docs/modules/legal.md). La logique est dans
// service ; les autres modules passent par contract.TermsGate.
package legal

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
	"github.com/thekrauss/kubemanager/internal/modules/legal/service"
)

// Controller : lecture publique, acceptation (organisations), rédaction (équipe).
type Controller struct {
	Service *service.Service
}

// lang : ?lang=fr|en, sinon la langue de la requête.
func lang(c *gin.Context, q string) (string, error) {
	l, ok := domain.ParseLang(q, string(i18n.From(c.Request.Context())))
	if !ok {
		return "", domain.ErrInvalidLegalQuery
	}
	return l, nil
}

// ── Public ──────────────────────────────────────────────────────────────

type ReadRequest struct {
	Kind    string `path:"kind" description:"Document : terms_vps (conditions de vente des VPS) ou aup (politique d'usage acceptable)"`
	Version int    `query:"version" description:"Numéro d'une version publiée (par défaut : la version en vigueur)"`
	Lang    string `query:"lang" description:"Langue du texte : fr ou en (par défaut : la langue de la requête)"`
}

// Read : version publiée d'un document, sans authentification.
func (h *Controller) Read(c *gin.Context, in *ReadRequest) (*service.Text, error) {
	l, err := lang(c, in.Lang)
	if err != nil {
		return nil, err
	}
	if in.Version < 0 {
		return nil, domain.ErrInvalidLegalQuery
	}
	return h.Service.Read(c.Request.Context(), strings.TrimSpace(in.Kind), in.Version, l)
}

// ── Organisations ───────────────────────────────────────────────────────

type OrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

func (p OrgPath) org() (uuid.UUID, error) {
	org, err := uuid.Parse(strings.TrimSpace(p.OrganizationID))
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return org, nil
}

type OrgStatusRequest struct {
	OrgPath
	Lang string `query:"lang" description:"Langue des titres : fr ou en (par défaut : la langue de la requête)"`
}

type OrgStatusResponse struct {
	Documents []service.OrgDocument `json:"documents" description:"Documents publiés et leur acceptation par l'organisation"`
}

// OrgStatus : pour chaque document publié, version en vigueur et version acceptée.
func (h *Controller) OrgStatus(c *gin.Context, in *OrgStatusRequest) (*OrgStatusResponse, error) {
	org, err := in.org()
	if err != nil {
		return nil, err
	}
	l, err := lang(c, in.Lang)
	if err != nil {
		return nil, err
	}
	docs, err := h.Service.OrgStatus(c.Request.Context(), org, l)
	if err != nil {
		return nil, err
	}
	return &OrgStatusResponse{Documents: docs}, nil
}

type AcceptRequest struct {
	OrgPath
	Kind    string `json:"kind" binding:"required" description:"Document accepté : terms_vps ou aup"`
	Version int    `json:"version" binding:"required" description:"Version acceptée : celle en vigueur"`
}

type AcceptResponse struct {
	Kind       string    `json:"kind" description:"Document accepté"`
	Version    int       `json:"version" description:"Version acceptée"`
	AcceptedAt time.Time `json:"accepted_at" description:"Date de l'acceptation"`
	AcceptedBy string    `json:"accepted_by" description:"Qui l'a acceptée"`
}

// Accept : l'organisation accepte la version en vigueur d'un document.
func (h *Controller) Accept(c *gin.Context, in *AcceptRequest) (*AcceptResponse, error) {
	org, err := in.org()
	if err != nil {
		return nil, err
	}
	by := ""
	if p, ok := security.PrincipalFromContext(c); ok {
		by = string(p)
	}
	a, err := h.Service.Accept(c.Request.Context(), org, strings.TrimSpace(in.Kind), in.Version, by, c.ClientIP())
	if err != nil {
		return nil, err
	}
	return &AcceptResponse{Kind: a.Kind, Version: a.Version, AcceptedAt: a.AcceptedAt, AcceptedBy: a.Principal}, nil
}

// ── Équipe (/admin/legal) ───────────────────────────────────────────────

type OverviewResponse struct {
	Documents []service.KindSummary `json:"documents"`
}

func (h *Controller) Overview(c *gin.Context) (*OverviewResponse, error) {
	list, err := h.Service.Overview(c.Request.Context())
	if err != nil {
		return nil, err
	}
	return &OverviewResponse{Documents: list}, nil
}

type KindPath struct {
	Kind string `path:"kind" description:"Document : terms_vps ou aup"`
}

type VersionsResponse struct {
	Versions []service.VersionSummary `json:"versions"`
}

func (h *Controller) Versions(c *gin.Context, in *KindPath) (*VersionsResponse, error) {
	list, err := h.Service.Versions(c.Request.Context(), in.Kind)
	if err != nil {
		return nil, err
	}
	return &VersionsResponse{Versions: list}, nil
}

type VersionPath struct {
	Kind    string `path:"kind" description:"Document : terms_vps ou aup"`
	Version int    `path:"version" description:"Numéro de la version"`
}

func (h *Controller) Version(c *gin.Context, in *VersionPath) (*service.VersionDetail, error) {
	return h.Service.Version(c.Request.Context(), in.Kind, in.Version)
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

type CreateDraftRequest struct {
	KindPath
	domain.Content
}

// CreateDraft : corps vide : copie de la version en vigueur.
func (h *Controller) CreateDraft(c *gin.Context, in *CreateDraftRequest) (*service.VersionDetail, error) {
	return h.Service.CreateDraft(c.Request.Context(), in.Kind, in.Content, principal(c))
}

type UpdateDraftRequest struct {
	VersionPath
	domain.Content
}

func (h *Controller) UpdateDraft(c *gin.Context, in *UpdateDraftRequest) (*service.VersionDetail, error) {
	return h.Service.UpdateDraft(c.Request.Context(), in.Kind, in.Version, in.Content, principal(c))
}

func (h *Controller) Publish(c *gin.Context, in *VersionPath) (*service.VersionDetail, error) {
	return h.Service.Publish(c.Request.Context(), in.Kind, in.Version, principal(c))
}

func (h *Controller) DeleteDraft(c *gin.Context, in *VersionPath) error {
	return h.Service.DeleteDraft(c.Request.Context(), in.Kind, in.Version)
}
