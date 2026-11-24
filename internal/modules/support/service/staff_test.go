package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

func TestAddNote_HiddenFromCustomer(t *testing.T) {
	s, _, box, org, client, staff := fixture()
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Site lent", Description: "Depuis ce matin."})
	require.NoError(t, err)
	id := v.Ticket.ID
	box.got = nil

	_, err = s.AddNote(ctx, id, staff, "  ")
	assert.ErrorIs(t, err, domain.ErrInvalidBody)
	sv, err := s.AddNote(ctx, id, staff, "Client déjà relancé la semaine dernière.")
	require.NoError(t, err)
	assert.Empty(t, box.got, "aucun e-mail")
	assert.Equal(t, domain.StatusOpen, sv.Ticket.Status, "statut inchangé")
	require.Len(t, sv.Messages, 1)
	assert.True(t, sv.Messages[0].Internal)
	assert.Equal(t, domain.EventNoteAdded, sv.Events[len(sv.Events)-1].Type)

	cv, err := s.Get(ctx, org, id)
	require.NoError(t, err)
	assert.Empty(t, cv.Messages, "la note n'est jamais montrée au client")
	for _, e := range cv.Events {
		assert.NotEqual(t, domain.EventNoteAdded, e.Type)
	}
}

func TestBulk(t *testing.T) {
	s, _, _, org, client, staff := fixture()
	ctx := context.Background()
	a, err := s.Create(ctx, org, client, CreateInput{Subject: "Premier", Description: "x"})
	require.NoError(t, err)
	b, err := s.Create(ctx, org, client, CreateInput{Subject: "Second", Description: "x"})
	require.NoError(t, err)

	_, err = s.Bulk(ctx, staff, BulkInput{IDs: []string{a.Ticket.ID.String()}})
	assert.Error(t, err, "rien à changer")
	_, err = s.Bulk(ctx, staff, BulkInput{IDs: make([]string, BulkMax+1), Priority: strPtr("HIGH")})
	assert.Error(t, err, "trop de tickets")

	missing := uuid.New().String()
	res, err := s.Bulk(ctx, staff, BulkInput{
		IDs:      []string{a.Ticket.ID.String(), missing, b.Ticket.ID.String(), a.Ticket.ID.String(), "pas-un-id"},
		Priority: strPtr("HIGH"), AssignTo: strPtr("me"),
	})
	require.NoError(t, err)
	assert.Equal(t, []BulkResult{
		{ID: a.Ticket.ID.String(), OK: true},
		{ID: missing, Error: "TICKET_NOT_FOUND"},
		{ID: b.Ticket.ID.String(), OK: true},
		{ID: "pas-un-id", Error: "TICKET_NOT_FOUND"},
	}, res.Results, "doublon ignoré, un échec n'arrête pas les autres")
	for _, id := range []uuid.UUID{a.Ticket.ID, b.Ticket.ID} {
		sv, err := s.AdminGet(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, domain.PriorityHigh, sv.Ticket.Priority)
		assert.Equal(t, staff.UserID, sv.Ticket.AssignedToUserID)
	}

	res, err = s.Bulk(ctx, staff, BulkInput{IDs: []string{a.Ticket.ID.String()}, Status: strPtr("NOPE")})
	require.NoError(t, err)
	assert.Equal(t, "INVALID_STATUS", res.Results[0].Error)
}

func TestAssignedFilter(t *testing.T) {
	me := uuid.New()
	var f domain.Filter
	require.NoError(t, assignedFilter("", &me, &f))
	assert.Nil(t, f.AssignedTo)
	assert.False(t, f.Unassigned)

	require.NoError(t, assignedFilter("me", &me, &f))
	assert.Equal(t, &me, f.AssignedTo)
	assert.Error(t, assignedFilter("me", nil, &domain.Filter{}), "compte de service")

	f = domain.Filter{}
	require.NoError(t, assignedFilter("none", &me, &f))
	assert.True(t, f.Unassigned)

	other := uuid.New()
	f = domain.Filter{}
	require.NoError(t, assignedFilter(other.String(), &me, &f))
	assert.Equal(t, &other, f.AssignedTo)
	assert.Error(t, assignedFilter("quelqu'un", &me, &domain.Filter{}))
}

func TestTicketContext_NeedsStaffStore(t *testing.T) {
	s, _, _, _, _, _ := fixture()
	_, err := s.TicketContext(context.Background(), uuid.New())
	assert.ErrorIs(t, err, ErrStaffUnavailable)
	_, err = s.Agents(context.Background())
	assert.ErrorIs(t, err, ErrStaffUnavailable)
}

func strPtr(s string) *string { return &s }

// staffFake : équipe fixe.
type staffFake struct{ agents []Agent }

func (f *staffFake) TicketFacts(context.Context, []domain.Ticket) (map[uuid.UUID]TicketFacts, error) {
	return map[uuid.UUID]TicketFacts{}, nil
}
func (f *staffFake) Agents(context.Context) ([]Agent, error) { return f.agents, nil }
func (f *staffFake) IsAgent(_ context.Context, id uuid.UUID) (bool, error) {
	for _, a := range f.agents {
		if a.UserID == id {
			return true, nil
		}
	}
	return false, nil
}
func (f *staffFake) Organization(context.Context, uuid.UUID) (*OrganizationContext, []ProjectContext, error) {
	return &OrganizationContext{}, nil, nil
}
func (f *staffFake) Resource(context.Context, string, uuid.UUID) (*ResourceContext, error) {
	return nil, nil
}

func TestStaffMails(t *testing.T) {
	s, store, box, org, client, staff := fixture()
	ctx := context.Background()
	mate := uuid.New()
	store.contacts[mate] = Contact{ID: mate, Email: "collegue@betorun.test", Locale: "fr"}
	team := &staffFake{agents: []Agent{{UserID: *staff.UserID}, {UserID: mate}}}
	s.Staff = team
	n := s.Notify.(*MailNotifier)
	n.Staff, n.StaffURL = team, "http://localhost:3001/"

	// Ticket normal : seulement l'accusé au client.
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Question", Description: "Comment faire ?"})
	require.NoError(t, err)
	require.Len(t, box.got, 1)
	box.got = nil

	// Ticket urgent : toute l'équipe, dans sa langue, avec la description et le lien du back-office.
	u, err := s.Create(ctx, org, client, CreateInput{Subject: "Tout est en panne", Description: "Erreur 502 partout", Priority: "URGENT"})
	require.NoError(t, err)
	to := map[string]mail.Message{}
	for _, m := range box.got {
		to[m.To] = m
	}
	require.Len(t, to, 3, "accusé au client + deux agents")
	assert.Equal(t, `Priority ticket: "Tout est en panne"`, to["agent@betorun.test"].Subject)
	assert.Equal(t, "Ticket prioritaire : « Tout est en panne »", to["collegue@betorun.test"].Subject)
	assert.Contains(t, to["collegue@betorun.test"].Text, "Erreur 502 partout")
	assert.Contains(t, to["collegue@betorun.test"].Text,
		"http://localhost:3001/admin/support?ticket="+u.Ticket.ID.String()+"&org="+org.String())

	// Assignation par soi-même : rien ; par un collègue : e-mail à l'agent ; même agent : rien.
	box.got = nil
	_, err = s.Assign(ctx, v.Ticket.ID, staff, staff.UserID)
	require.NoError(t, err)
	assert.Empty(t, box.got)
	matey := ActorOf("user:" + mate.String())
	_, err = s.Assign(ctx, v.Ticket.ID, staff, &mate)
	require.NoError(t, err)
	require.Len(t, box.got, 1)
	assert.Equal(t, "collegue@betorun.test", box.got[0].To)
	assert.Equal(t, "Ticket assigné : « Question »", box.got[0].Subject)
	box.got = nil
	_, err = s.Assign(ctx, v.Ticket.ID, matey, &mate)
	require.NoError(t, err)
	assert.Empty(t, box.got, "déjà assigné")

	// Message du client : l'agent assigné, avec la citation.
	_, err = s.Reply(ctx, org, v.Ticket.ID, client, "Pouvez-vous m'aider ?")
	require.NoError(t, err)
	require.Len(t, box.got, 1)
	assert.Equal(t, "collegue@betorun.test", box.got[0].To)
	assert.Contains(t, box.got[0].Text, "Pouvez-vous m'aider ?")

	// Sans adresse du back-office : aucun e-mail à l'équipe.
	box.got = nil
	n.StaffURL = ""
	_, err = s.Reply(ctx, org, v.Ticket.ID, client, "Relance")
	require.NoError(t, err)
	assert.Empty(t, box.got)
}
