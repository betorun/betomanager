package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// Publisher : publication sur un canal temps réel (Centrifugo).
type Publisher interface {
	Publish(ctx context.Context, channel string, data any) error
}

// Sortes de changement publiées sur support:ticket:<ticket>.
const (
	LiveMessage    = "message"
	LiveStatus     = "status"
	LiveAttachment = "attachment"
	LiveAssign     = "assign"
)

// LiveEvent : signal minimal, sans contenu : la console relit le ticket avec
// ses propres droits (aucun message ne transite par le canal).
type LiveEvent struct {
	TicketID   string `json:"ticket_id"`
	Kind       string `json:"kind"`
	AuthorRole string `json:"author_role,omitempty"`
	At         string `json:"at"`
}

// Sortes publiées sur le fil de l'équipe (support:staff).
const (
	FeedTicketCreated   = "ticket_created"
	FeedCustomerMessage = "customer_message"
)

// Sortes publiées sur le fil d'une organisation (support:org:<org>).
const (
	OrgAgentMessage = "agent_message"
	OrgStatus       = "status"
)

// StaffFeedEvent : de quoi afficher une notification à l'équipe sans relire
// le ticket (le fil n'est ouvert qu'au staff).
type StaffFeedEvent struct {
	Kind             string     `json:"kind"`
	TicketID         string     `json:"ticket_id"`
	OrganizationID   string     `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	Subject          string     `json:"subject"`
	Priority         string     `json:"priority"`
	Status           string     `json:"status"`
	AuthorEmail      string     `json:"author_email,omitempty"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	At               string     `json:"at"`
}

// OrgFeedEvent : notification d'un client (lecteurs des tickets de
// l'organisation). Jamais le contenu d'un message, jamais une note interne.
type OrgFeedEvent struct {
	Kind     string `json:"kind"`
	TicketID string `json:"ticket_id"`
	Subject  string `json:"subject"`
	Status   string `json:"status"`
	At       string `json:"at"`
}

// publish : après l'enregistrement ; une erreur est journalisée, jamais rendue.
func (s *Service) publish(ctx context.Context, t *domain.Ticket, kind string, role domain.AuthorRole) {
	if s.Live == nil {
		return
	}
	ev := LiveEvent{TicketID: t.ID.String(), Kind: kind, At: s.stamp()}
	switch role {
	case domain.AuthorCustomer:
		ev.AuthorRole = "customer"
	case domain.AuthorSupportAgent:
		ev.AuthorRole = "agent"
	}
	s.emit(ctx, realtime.SupportTicketChannel(t.ID.String()), ev, t, kind)
}

// publishStaff : fil de l'équipe. author : personne à l'origine (client).
func (s *Service) publishStaff(ctx context.Context, t *domain.Ticket, kind string, author *uuid.UUID) {
	if s.Live == nil {
		return
	}
	ev := StaffFeedEvent{Kind: kind, TicketID: t.ID.String(), OrganizationID: t.OrganizationID.String(),
		Subject: t.Subject, Priority: string(t.Priority), Status: string(t.Status),
		AssignedToUserID: t.AssignedToUserID, At: s.stamp()}
	if s.Staff != nil {
		if facts, err := s.Staff.TicketFacts(ctx, []domain.Ticket{*t}); err == nil {
			ev.OrganizationName = facts[t.ID].OrganizationName
		}
	}
	if author != nil {
		if cs, err := s.Store.Contacts(ctx, []uuid.UUID{*author}); err == nil && len(cs) == 1 {
			ev.AuthorEmail = cs[0].Email
		}
	}
	s.emit(ctx, realtime.SupportStaffChannel, ev, t, kind)
}

// publishOrg : fil de l'organisation du ticket.
func (s *Service) publishOrg(ctx context.Context, t *domain.Ticket, kind string) {
	if s.Live == nil {
		return
	}
	ev := OrgFeedEvent{Kind: kind, TicketID: t.ID.String(), Subject: t.Subject, Status: string(t.Status), At: s.stamp()}
	s.emit(ctx, realtime.SupportOrgChannel(t.OrganizationID.String()), ev, t, kind)
}

func (s *Service) stamp() string { return s.now().UTC().Format(time.RFC3339) }

func (s *Service) emit(ctx context.Context, channel string, data any, t *domain.Ticket, kind string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.Live.Publish(pctx, channel, data); err != nil && s.Logger != nil {
		s.Logger.Warnw("support: live publish failed", "channel", channel, "ticket", t.ID, "kind", kind, "error", err)
	}
}
