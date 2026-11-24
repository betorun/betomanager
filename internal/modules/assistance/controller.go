package assistance

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

// Controller : routes du client (/organizations/:id/assistance) et de
// l'équipe (/admin/support/tickets/:ticketID/assistance, /admin/assistance).
type Controller struct {
	Service *Service
}

// ListResponse : attributions.
type ListResponse struct {
	Items []Grant `json:"items"`
}

func person(c *gin.Context) (Person, error) {
	id, ok := security.InternalUserID(c)
	if !ok {
		return Person{}, betoerrors.New(betoerrors.CodeForbidden, "a signed-in person is required")
	}
	return Person{ID: id, Email: c.GetString("email")}, nil
}

// ── Client ──────────────────────────────────────────────────────────────

// OrgInput : organisation.
type OrgInput struct {
	OrganizationID uuid.UUID `path:"id" description:"Organisation"`
}

// List : les accès de l'équipe sur l'organisation (demandes, actifs, passés).
func (h *Controller) List(c *gin.Context, in *OrgInput) (*ListResponse, error) {
	items, err := h.Service.List(c.Request.Context(), in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return &ListResponse{Items: items}, nil
}

// ApproveInput : accepter une demande.
type ApproveInput struct {
	OrganizationID uuid.UUID `path:"id" description:"Organisation"`
	GrantID        uuid.UUID `path:"grantID" description:"Accès d'assistance"`
	Hours          int       `json:"hours" validate:"omitempty,min=1,max=24" description:"Durée en heures (1 à 24) ; défaut : la durée demandée"`
}

// Approve : un propriétaire accepte (double authentification récente).
func (h *Controller) Approve(c *gin.Context, in *ApproveInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.Approve(c.Request.Context(), in.OrganizationID, in.GrantID, p, in.Hours)
	return out(g, err)
}

// DecisionInput : refuser ou retirer.
type DecisionInput struct {
	OrganizationID uuid.UUID `path:"id" description:"Organisation"`
	GrantID        uuid.UUID `path:"grantID" description:"Accès d'assistance"`
	Note           string    `json:"note" validate:"max=500" description:"Raison (facultative)"`
}

// Deny : refuser une demande.
func (h *Controller) Deny(c *gin.Context, in *DecisionInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.Deny(c.Request.Context(), in.OrganizationID, in.GrantID, p, in.Note)
	return out(g, err)
}

// Revoke : retirer un accès actif (effet immédiat).
func (h *Controller) Revoke(c *gin.Context, in *DecisionInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.Revoke(c.Request.Context(), in.OrganizationID, in.GrantID, p, in.Note)
	return out(g, err)
}

// GrantTicketInput : ouvrir l'accès à l'agent d'un ticket.
type GrantTicketInput struct {
	OrganizationID uuid.UUID `path:"id" description:"Organisation"`
	TicketID       uuid.UUID `path:"ticketID" description:"Ticket concerné"`
	Reason         string    `json:"reason" validate:"required,min=20,max=500" description:"Pourquoi l'équipe doit intervenir"`
	Hours          int       `json:"hours" validate:"omitempty,min=1,max=24" description:"Durée en heures (1 à 24, défaut 4)"`
}

// GrantForTicket : un propriétaire ouvre l'accès de lui-même, à l'agent
// assigné au ticket.
func (h *Controller) GrantForTicket(c *gin.Context, in *GrantTicketInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.GrantForTicket(c.Request.Context(), in.OrganizationID, in.TicketID, p, in.Reason, in.Hours)
	return out(g, err)
}

// ── Équipe ──────────────────────────────────────────────────────────────

// RequestInput : demande depuis un ticket.
type RequestInput struct {
	TicketID uuid.UUID `path:"ticketID" description:"Ticket concerné"`
	Reason   string    `json:"reason" validate:"required,min=20,max=500" description:"Ce que l'équipe doit faire chez le client, et pourquoi"`
	Hours    int       `json:"hours" validate:"omitempty,min=1,max=24" description:"Durée souhaitée en heures (1 à 24, défaut 4)"`
}

// Request : la personne demande l'accès au client du ticket.
func (h *Controller) Request(c *gin.Context, in *RequestInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	org, err := h.Service.TicketOrganization(c.Request.Context(), in.TicketID)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.Request(c.Request.Context(), p, org, &in.TicketID, in.Reason, in.Hours)
	return out(g, err)
}

// BreakGlassInput : accès d'urgence.
type BreakGlassInput struct {
	TicketID uuid.UUID `path:"ticketID" description:"Ticket concerné"`
	Reason   string    `json:"reason" validate:"required,min=20,max=500" description:"L'incident : ce qui menace la plateforme ou les données du client, et pourquoi le client ne peut pas être joint"`
	Minutes  int       `json:"minutes" validate:"omitempty,min=5,max=60" description:"Durée en minutes (5 à 60, défaut 60)"`
}

// BreakGlass : accès sans l'accord du client (double authentification
// récente ; propriétaires prévenus tout de suite).
func (h *Controller) BreakGlass(c *gin.Context, in *BreakGlassInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	org, err := h.Service.TicketOrganization(c.Request.Context(), in.TicketID)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.BreakGlass(c.Request.Context(), p, org, &in.TicketID, in.Reason, time.Duration(in.Minutes)*time.Minute)
	return out(g, err)
}

// Mine : les accès de la personne de l'équipe.
func (h *Controller) Mine(c *gin.Context) (*ListResponse, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	items, err := h.Service.ListStaff(c.Request.Context(), &p.ID)
	if err != nil {
		return nil, err
	}
	return &ListResponse{Items: items}, nil
}

// CloseInput : clore son accès.
type CloseInput struct {
	GrantID uuid.UUID `path:"grantID" description:"Accès d'assistance"`
	Note    string    `json:"note" validate:"max=500" description:"Ce qui a été fait (facultatif)"`
}

// Close : la personne clôt son accès ou retire sa demande.
func (h *Controller) Close(c *gin.Context, in *CloseInput) (*Grant, error) {
	p, err := person(c)
	if err != nil {
		return nil, err
	}
	g, err := h.Service.Close(c.Request.Context(), p, in.GrantID, in.Note)
	return out(g, err)
}

// GrantOrganization : organisation d'un accès (cible du journal du client
// pour les routes de l'équipe).
func (s *Service) GrantOrganization(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var g Grant
	if err := s.DB.WithContext(ctx).Select("organization_id").Where("id = ?", id).First(&g).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, err
	}
	return g.OrganizationID, nil
}

// TicketOrganization : organisation d'un ticket.
func (s *Service) TicketOrganization(ctx context.Context, ticket uuid.UUID) (uuid.UUID, error) {
	var org uuid.UUID
	if err := s.DB.WithContext(ctx).Raw(`SELECT organization_id FROM support_tickets WHERE id = ?`, ticket).Scan(&org).Error; err != nil {
		return uuid.Nil, err
	}
	if org == uuid.Nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "ticket not found")
	}
	return org, nil
}

func out(g Grant, err error) (*Grant, error) {
	if err != nil {
		return nil, err
	}
	return &g, nil
}
