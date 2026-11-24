package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type liveSpy struct {
	channels []string
	events   []LiveEvent
	fail     bool
}

func (l *liveSpy) Publish(_ context.Context, channel string, data any) error {
	l.channels = append(l.channels, channel)
	l.events = append(l.events, data.(LiveEvent))
	if l.fail {
		return errors.New("centrifugo indisponible")
	}
	return nil
}

func TestLive_PublishesEachChange(t *testing.T) {
	s, _, _, org, client, staff := fixture()
	spy := &liveSpy{}
	s.Live = spy
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Facture", Description: "Montant incorrect"})
	require.NoError(t, err)
	id := v.Ticket.ID
	assert.Empty(t, spy.events, "création : personne n'écoute encore ce ticket")

	_, err = s.Reply(ctx, org, id, client, "Toujours rien")
	require.NoError(t, err)
	_, err = s.AdminReply(ctx, id, staff, "Corrigé")
	require.NoError(t, err)
	_, err = s.Assign(ctx, id, staff, staff.UserID)
	require.NoError(t, err)
	status := "IN_PROGRESS"
	_, err = s.AdminUpdate(ctx, id, staff, UpdateInput{Status: &status})
	require.NoError(t, err)
	_, err = s.Attach(ctx, org, id, client, AttachInput{FileName: "a.txt", Size: 5, Content: bytes.NewReader([]byte("hello"))})
	require.NoError(t, err)
	_, err = s.Resolve(ctx, org, id, client)
	require.NoError(t, err)

	var kinds, roles []string
	for i, ev := range spy.events {
		assert.Equal(t, "support:ticket:"+id.String(), spy.channels[i])
		assert.Equal(t, id.String(), ev.TicketID)
		assert.NotEmpty(t, ev.At)
		kinds, roles = append(kinds, ev.Kind), append(roles, ev.AuthorRole)
	}
	assert.Equal(t, []string{"message", "message", "assign", "status", "attachment", "status"}, kinds)
	assert.Equal(t, []string{"customer", "agent", "", "", "", ""}, roles)
}

func TestLive_FailOpen(t *testing.T) {
	s, _, _, org, client, _ := fixture()
	s.Live = &liveSpy{fail: true}
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Facture", Description: "Montant incorrect"})
	require.NoError(t, err)
	_, err = s.Reply(ctx, org, v.Ticket.ID, client, "Relance")
	assert.NoError(t, err, "Centrifugo en panne ne fait pas échouer la réponse")
}
