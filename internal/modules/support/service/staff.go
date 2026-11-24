package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// Back-office du support : ce que l'équipe voit en plus du client (contexte
// de l'organisation, agents, délais, notes internes, actions groupées).

// StaffStore : lectures propres au back-office (Postgres).
type StaffStore interface {
	// TicketFacts : organisation, agent assigné et délais de chaque ticket.
	TicketFacts(ctx context.Context, tickets []domain.Ticket) (map[uuid.UUID]TicketFacts, error)
	// Agents : comptes du staff (rôle sur la plateforme, directement ou par un groupe).
	Agents(ctx context.Context) ([]Agent, error)
	// IsAgent : la personne fait partie du staff.
	IsAgent(ctx context.Context, user uuid.UUID) (bool, error)
	// Organization : identité, propriétaire et projets (avec leurs ressources).
	Organization(ctx context.Context, org uuid.UUID) (*OrganizationContext, []ProjectContext, error)
	// Resource : ressource rattachée au ticket (genre connu : "vps") ; nil si inconnue.
	Resource(ctx context.Context, kind string, id uuid.UUID) (*ResourceContext, error)
}

// Agent : membre de l'équipe.
type Agent struct {
	UserID   uuid.UUID `json:"user_id"`
	Email    string    `json:"email"`
	FullName string    `json:"full_name"`
}

// TicketFacts : ce que la file de l'équipe montre en plus d'un ticket.
type TicketFacts struct {
	OrganizationName      string
	AssignedTo            *Agent
	LastCustomerMessageAt *time.Time
	FirstResponseAt       *time.Time
}

// StaffTicket : ticket vu par l'équipe.
type StaffTicket struct {
	domain.Ticket
	OrganizationName string `json:"organization_name,omitempty" description:"Nom de l'organisation"`
	AssignedTo       *Agent `json:"assigned_to,omitempty" description:"Agent en charge"`
	// LastCustomerMessageAt : dernier message du client (création comprise).
	LastCustomerMessageAt *time.Time `json:"last_customer_message_at,omitempty" description:"Dernier message du client"`
	// FirstResponseAt : première réponse de l'équipe ; absente si aucune.
	FirstResponseAt *time.Time `json:"first_response_at,omitempty" description:"Première réponse de l'équipe"`
}

// StaffTicketList : file de l'équipe.
type StaffTicketList struct {
	Items []StaffTicket `json:"items"`
	Total int64         `json:"total"`
}

// StaffTicketView : ticket complet pour l'équipe (notes internes comprises).
type StaffTicketView struct {
	Ticket      *StaffTicket        `json:"ticket"`
	Messages    []domain.Message    `json:"messages"`
	Attachments []domain.Attachment `json:"attachments"`
	Events      []domain.Event      `json:"events"`
}

// OrganizationContext : l'organisation d'un ticket.
type OrganizationContext struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Owner     *Person   `json:"owner,omitempty"`
}

// Person : compte d'un client.
type Person struct {
	UserID   uuid.UUID `json:"user_id"`
	Email    string    `json:"email"`
	FullName string    `json:"full_name"`
}

// ProjectContext : un projet et le nombre de ses ressources.
type ProjectContext struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Workloads int64     `json:"workloads"`
	Sites     int64     `json:"sites"`
	VPS       int64     `json:"vps"`
	Databases int64     `json:"databases"`
}

// ResourceContext : ressource rattachée au ticket.
type ResourceContext struct {
	Kind      string    `json:"kind"`
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	ProjectID uuid.UUID `json:"project_id"`
}

// BillingContext : porte-monnaie de l'organisation.
type BillingContext struct {
	Balance  int64  `json:"balance"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// TicketContext : tout ce qu'il faut savoir du client pour répondre.
type TicketContext struct {
	Organization *OrganizationContext `json:"organization"`
	Billing      *BillingContext      `json:"billing,omitempty"`
	Projects     []ProjectContext     `json:"projects"`
	Resource     *ResourceContext     `json:"resource,omitempty"`
	OtherTickets []domain.Ticket      `json:"other_tickets"`
}

// otherTicketsMax : autres tickets de l'organisation montrés dans le contexte.
const otherTicketsMax = 10

// BulkMax : tickets par action groupée.
const BulkMax = 50

// ErrStaffUnavailable : back-office sans ses lectures (configuration).
var ErrStaffUnavailable = errors.New("support back-office is not available")

// staffTickets : tickets enrichis pour l'équipe.
func (s *Service) staffTickets(ctx context.Context, list []domain.Ticket) ([]StaffTicket, error) {
	out := make([]StaffTicket, 0, len(list))
	facts := map[uuid.UUID]TicketFacts{}
	if s.Staff != nil && len(list) > 0 {
		var err error
		if facts, err = s.Staff.TicketFacts(ctx, list); err != nil {
			return nil, err
		}
	}
	for _, t := range list {
		if t.Tags == nil {
			t.Tags = []string{}
		}
		f := facts[t.ID]
		out = append(out, StaffTicket{Ticket: t, OrganizationName: f.OrganizationName, AssignedTo: f.AssignedTo,
			LastCustomerMessageAt: f.LastCustomerMessageAt, FirstResponseAt: f.FirstResponseAt})
	}
	return out, nil
}

// staffView : ticket complet pour l'équipe.
func (s *Service) staffView(ctx context.Context, t *domain.Ticket) (*StaffTicketView, error) {
	v, err := s.fullView(ctx, t)
	if err != nil {
		return nil, err
	}
	st, err := s.staffTickets(ctx, []domain.Ticket{*v.Ticket})
	if err != nil {
		return nil, err
	}
	return &StaffTicketView{Ticket: &st[0], Messages: v.Messages, Attachments: v.Attachments, Events: v.Events}, nil
}

// staffAgain : vue de l'équipe après une action (relue).
func (s *Service) staffAgain(ctx context.Context, id uuid.UUID, err error) (*StaffTicketView, error) {
	if err != nil {
		return nil, err
	}
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.staffView(ctx, t)
}

// AssignedFilter : « me », « none » ou l'identifiant d'un agent.
func assignedFilter(raw string, me *uuid.UUID, f *domain.Filter) error {
	switch raw = strings.TrimSpace(raw); raw {
	case "":
		return nil
	case "none":
		f.Unassigned = true
		return nil
	case "me":
		if me == nil {
			return betoerrors.New(betoerrors.CodeInvalidInput, "assigned=me needs a person")
		}
		f.AssignedTo = me
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return betoerrors.New(betoerrors.CodeInvalidInput, "assigned must be me, none or an agent id")
	}
	f.AssignedTo = &id
	return nil
}

// TicketContext : organisation, porte-monnaie, projets, ressource rattachée
// et autres tickets récents du client.
func (s *Service) TicketContext(ctx context.Context, id uuid.UUID) (*TicketContext, error) {
	if s.Staff == nil {
		return nil, ErrStaffUnavailable
	}
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	org, projects, err := s.Staff.Organization(ctx, t.OrganizationID)
	if err != nil {
		return nil, err
	}
	if projects == nil {
		projects = []ProjectContext{}
	}
	out := &TicketContext{Organization: org, Projects: projects, OtherTickets: []domain.Ticket{}}
	if s.Wallet != nil {
		if b, err := s.Wallet(ctx, t.OrganizationID); err == nil {
			out.Billing = b
		} else if s.Logger != nil {
			s.Logger.Warnw("support: wallet not read", "organization", t.OrganizationID, "error", err)
		}
	}
	if t.ResourceKind != "" && t.ResourceID != nil {
		if out.Resource, err = s.Staff.Resource(ctx, t.ResourceKind, *t.ResourceID); err != nil {
			return nil, err
		}
	}
	others, _, err := s.Store.ListTickets(ctx, domain.Filter{OrganizationID: &t.OrganizationID, Limit: otherTicketsMax + 1})
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.ID != t.ID && len(out.OtherTickets) < otherTicketsMax {
			if o.Tags == nil {
				o.Tags = []string{}
			}
			out.OtherTickets = append(out.OtherTickets, o)
		}
	}
	return out, nil
}

// Agents : membres de l'équipe, pour l'assignation.
func (s *Service) Agents(ctx context.Context) ([]Agent, error) {
	if s.Staff == nil {
		return nil, ErrStaffUnavailable
	}
	list, err := s.Staff.Agents(ctx)
	if list == nil {
		list = []Agent{}
	}
	return list, err
}

// AddNote : note interne de l'équipe ; ni e-mail, ni statut changé, ni
// publication sur le canal du client.
func (s *Service) AddNote(ctx context.Context, id uuid.UUID, by Actor, body string) (*StaffTicketView, error) {
	body, err := domain.CleanBody(body, domain.ErrInvalidBody)
	if err != nil {
		return nil, err
	}
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	msg := &domain.Message{ID: uuid.New(), TicketID: t.ID, OrganizationID: t.OrganizationID, AuthorUserID: by.UserID,
		AuthorRole: domain.AuthorSupportAgent, Internal: true, Body: body, CreatedAt: now, UpdatedAt: now}
	ev := s.event(t, domain.EventNoteAdded, by, nil)
	t.UpdatedAt = now
	if err := s.Store.SaveTicket(ctx, t, msg, []domain.Event{ev}); err != nil {
		return nil, err
	}
	return s.staffView(ctx, t)
}

// BulkInput : même changement sur plusieurs tickets (champs absents : inchangés).
type BulkInput struct {
	IDs      []string `json:"ids" binding:"required" description:"Tickets visés (50 au plus)"`
	Status   *string  `json:"status,omitempty" enum:"OPEN,IN_PROGRESS,WAITING_CUSTOMER,RESOLVED,CLOSED" description:"Statut"`
	Priority *string  `json:"priority,omitempty" enum:"LOW,MEDIUM,HIGH,URGENT" description:"Priorité"`
	// AssignTo : identifiant d'un agent, « me » ou « none » (retirer).
	AssignTo *string `json:"assign_to,omitempty" description:"Agent en charge : identifiant, me ou none"`
}

// BulkResult : résultat pour un ticket (Error : code d'erreur, vide si réussi).
type BulkResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// BulkResponse : un résultat par ticket, dans l'ordre demandé.
type BulkResponse struct {
	Results []BulkResult `json:"results"`
}

// Bulk : chaque ticket est traité seul (un échec n'arrête pas les autres) ;
// chaque changement est inscrit dans son journal, comme une action unitaire.
func (s *Service) Bulk(ctx context.Context, by Actor, in BulkInput) (*BulkResponse, error) {
	if len(in.IDs) == 0 || len(in.IDs) > BulkMax {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "ids must hold 1 to 50 tickets")
	}
	if in.Status == nil && in.Priority == nil && in.AssignTo == nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "nothing to change")
	}
	var assign *uuid.UUID
	if in.AssignTo != nil {
		switch raw := strings.TrimSpace(*in.AssignTo); raw {
		case "none":
		case "me":
			if by.UserID == nil {
				return nil, betoerrors.New(betoerrors.CodeInvalidInput, "assign_to=me needs a person")
			}
			assign = by.UserID
		default:
			id, err := uuid.Parse(raw)
			if err != nil {
				return nil, domain.ErrInvalidAssignee
			}
			assign = &id
		}
	}
	out := &BulkResponse{Results: make([]BulkResult, 0, len(in.IDs))}
	seen := map[uuid.UUID]bool{}
	for _, raw := range in.IDs {
		res := BulkResult{ID: raw}
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			err = domain.ErrTicketNotFound
		} else if seen[id] {
			continue
		} else {
			seen[id] = true
			err = s.bulkOne(ctx, id, by, in, assign)
		}
		if err != nil {
			res.Error = errorCode(err)
		} else {
			res.OK = true
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

func (s *Service) bulkOne(ctx context.Context, id uuid.UUID, by Actor, in BulkInput, assign *uuid.UUID) error {
	if in.Status != nil || in.Priority != nil {
		if _, err := s.AdminUpdate(ctx, id, by, UpdateInput{Status: in.Status, Priority: in.Priority}); err != nil {
			return err
		}
	}
	if in.AssignTo != nil {
		if _, err := s.Assign(ctx, id, by, assign); err != nil {
			return err
		}
	}
	return nil
}

// errorCode : code stable d'une erreur pour un résultat groupé.
func errorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrTicketNotFound):
		return "TICKET_NOT_FOUND"
	case errors.Is(err, domain.ErrInvalidAssignee):
		return "INVALID_ASSIGNEE"
	case errors.Is(err, domain.ErrInvalidStatus):
		return "INVALID_STATUS"
	case errors.Is(err, domain.ErrInvalidPriority):
		return "INVALID_PRIORITY"
	}
	return "INTERNAL"
}
