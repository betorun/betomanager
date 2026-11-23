// Package status : page de statut publique et incidents (équipe), routes
// HTTP (docs/modules/status.md).
package status

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
	"github.com/thekrauss/kubemanager/internal/modules/status/service"
)

// Controller : GET /status (public) et /admin/status (équipe).
type Controller struct {
	Service *service.Service
}

type PublicRequest struct {
	Lang string `query:"lang" description:"Langue des textes : fr ou en (par défaut : la langue de la requête)"`
}

// Public : page de statut, sans authentification ; mise en cache 30 s.
func (h *Controller) Public(c *gin.Context, in *PublicRequest) (*service.PublicStatus, error) {
	lang := i18n.From(c.Request.Context())
	switch strings.ToLower(strings.TrimSpace(in.Lang)) {
	case string(i18n.FR):
		lang = i18n.FR
	case string(i18n.EN):
		lang = i18n.EN
	}
	out, err := h.Service.Public(c.Request.Context(), lang)
	if err != nil {
		return nil, err
	}
	c.Header("Cache-Control", "public, max-age=30")
	c.Header("Vary", "Accept-Language, Cookie")
	return out, nil
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

// Overview : états des composants avec la raison de chaque sonde (équipe).
func (h *Controller) Overview(c *gin.Context) (*service.AdminStatus, error) {
	return h.Service.Admin(c.Request.Context())
}

type ListRequest struct {
	Open  bool `query:"open" description:"Seulement les incidents non résolus"`
	Limit int  `query:"limit" description:"1 à 200 (défaut 50)"`
}

type IncidentsResponse struct {
	Incidents []domain.Incident `json:"incidents"`
}

func (h *Controller) List(c *gin.Context, in *ListRequest) (*IncidentsResponse, error) {
	list, err := h.Service.List(c.Request.Context(), in.Open, in.Limit)
	if err != nil {
		return nil, err
	}
	return &IncidentsResponse{Incidents: list}, nil
}

type IncidentPath struct {
	IncidentID string `path:"incidentID" description:"Identifiant de l'incident"`
}

func (p IncidentPath) id() (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(p.IncidentID))
	if err != nil {
		return uuid.Nil, domain.ErrIncidentNotFound
	}
	return id, nil
}

func (h *Controller) Get(c *gin.Context, in *IncidentPath) (*domain.Incident, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.Get(c.Request.Context(), id)
}

func (h *Controller) Create(c *gin.Context, in *domain.IncidentInput) (*domain.Incident, error) {
	return h.Service.Create(c.Request.Context(), *in, principal(c))
}

type EditRequest struct {
	IncidentPath
	domain.IncidentInput
}

func (h *Controller) Edit(c *gin.Context, in *EditRequest) (*domain.Incident, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.Edit(c.Request.Context(), id, in.IncidentInput, principal(c))
}

type AddUpdateRequest struct {
	IncidentPath
	domain.UpdateInput
}

func (h *Controller) AddUpdate(c *gin.Context, in *AddUpdateRequest) (*domain.Incident, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.AddUpdate(c.Request.Context(), id, in.UpdateInput, principal(c))
}

type ResolveRequest struct {
	IncidentPath
	service.ResolveInput
}

func (h *Controller) Resolve(c *gin.Context, in *ResolveRequest) (*domain.Incident, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.Resolve(c.Request.Context(), id, in.ResolveInput, principal(c))
}
