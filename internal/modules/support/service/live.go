package service

import (
	"context"
	"time"

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

// publish : après l'enregistrement ; une erreur est journalisée, jamais rendue.
func (s *Service) publish(ctx context.Context, t *domain.Ticket, kind string, role domain.AuthorRole) {
	if s.Live == nil {
		return
	}
	ev := LiveEvent{TicketID: t.ID.String(), Kind: kind, At: s.now().UTC().Format(time.RFC3339)}
	switch role {
	case domain.AuthorCustomer:
		ev.AuthorRole = "customer"
	case domain.AuthorSupportAgent:
		ev.AuthorRole = "agent"
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.Live.Publish(pctx, realtime.SupportTicketChannel(t.ID.String()), ev); err != nil && s.Logger != nil {
		s.Logger.Warnw("support: live publish failed", "ticket", t.ID, "kind", kind, "error", err)
	}
}
