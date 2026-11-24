package service

import (
	"context"
	"io"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// Store : tickets et réponses types (Postgres).
type Store interface {
	// CreateTicket : le ticket et son événement de création, ensemble.
	CreateTicket(ctx context.Context, t *domain.Ticket, ev domain.Event) error
	// SaveTicket : le ticket, un message éventuel et des événements, ensemble.
	SaveTicket(ctx context.Context, t *domain.Ticket, msg *domain.Message, events []domain.Event) error
	// Ticket : ErrTicketNotFound s'il n'existe pas.
	Ticket(ctx context.Context, id uuid.UUID) (*domain.Ticket, error)
	ListTickets(ctx context.Context, f domain.Filter) ([]domain.Ticket, int64, error)
	Messages(ctx context.Context, ticket uuid.UUID) ([]domain.Message, error)
	Events(ctx context.Context, ticket uuid.UUID) ([]domain.Event, error)
	Attachments(ctx context.Context, ticket uuid.UUID) ([]domain.Attachment, error)
	// AddAttachment : ErrAttachmentLimit si le ticket en a déjà max (compté
	// sous verrou du ticket : deux envois simultanés ne dépassent pas).
	AddAttachment(ctx context.Context, a *domain.Attachment, max int) error
	// Attachment : ErrAttachmentNotFound s'il n'est pas sur ce ticket.
	Attachment(ctx context.Context, ticket, id uuid.UUID) (*domain.Attachment, error)
	MessageOnTicket(ctx context.Context, ticket, msg uuid.UUID) (bool, error)
	ProjectInOrganization(ctx context.Context, project, org uuid.UUID) (bool, error)
	// ResourceInOrganization : la ressource rattachée (genre connu : "vps")
	// appartient à l'organisation ; un genre inconnu est refusé.
	ResourceInOrganization(ctx context.Context, kind string, id, org uuid.UUID) (bool, error)
	// Contacts : comptes actifs parmi ces personnes.
	Contacts(ctx context.Context, users []uuid.UUID) ([]Contact, error)
	// MemberContacts : comme Contacts, mais seulement les personnes qui ont
	// encore un rôle dans l'organisation (directement ou par un groupe) :
	// une personne retirée ne reçoit plus le contenu des tickets.
	MemberContacts(ctx context.Context, org uuid.UUID, users []uuid.UUID) ([]Contact, error)
	UserExists(ctx context.Context, user uuid.UUID) (bool, error)
	// Summary : compteurs de la file ; me : agent qui demande (compteur « mine »).
	Summary(ctx context.Context, me *uuid.UUID) (domain.Summary, error)

	Macros(ctx context.Context) ([]domain.Macro, error)
	CreateMacro(ctx context.Context, m *domain.Macro) error
	// UpdateMacro / DeleteMacro : ErrMacroNotFound si elle n'existe pas.
	UpdateMacro(ctx context.Context, m *domain.Macro) error
	DeleteMacro(ctx context.Context, id uuid.UUID) error
}

// Contact : destinataire d'un e-mail.
type Contact struct {
	ID       uuid.UUID
	Email    string
	FullName string
	// Locale : "fr", "en" ou "" (langue de la requête).
	Locale string
}

// Files : stockage des pièces jointes (objectstore.Store).
type Files interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// Notifier : e-mails au client. Jamais d'erreur rendue : un envoi raté ne
// fait pas échouer la requête.
type Notifier interface {
	Received(ctx context.Context, t *domain.Ticket)
	AgentReplied(ctx context.Context, t *domain.Ticket, m *domain.Message, to []uuid.UUID)
	Resolved(ctx context.Context, t *domain.Ticket)
}
