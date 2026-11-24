package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
