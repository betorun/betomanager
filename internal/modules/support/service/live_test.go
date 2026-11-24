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
	events   []LiveEvent // canaux support:ticket:<id>
	staff    []StaffFeedEvent
	org      []OrgFeedEvent
	orgChans []string
	fail     bool
}

func (l *liveSpy) Publish(_ context.Context, channel string, data any) error {
	switch ev := data.(type) {
	case LiveEvent:
		l.channels = append(l.channels, channel)
		l.events = append(l.events, ev)
	case StaffFeedEvent:
		if channel != "support:staff" {
			panic(channel)
		}
		l.staff = append(l.staff, ev)
	case OrgFeedEvent:
		l.orgChans = append(l.orgChans, channel)
		l.org = append(l.org, ev)
	}
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

func TestLive_StaffAndOrgFeeds(t *testing.T) {
	s, _, _, org, client, staff := fixture()
	spy := &liveSpy{}
	s.Live = spy
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Site en panne", Description: "502", Priority: "URGENT"})
	require.NoError(t, err)
	id := v.Ticket.ID
	_, err = s.Reply(ctx, org, id, client, "Toujours en panne")
	require.NoError(t, err)
	_, err = s.AddNote(ctx, id, staff, "Voir le nœud 2")
	require.NoError(t, err)
	_, err = s.AdminReply(ctx, id, staff, "Relancé")
	require.NoError(t, err)
	resolved := "RESOLVED"
	_, err = s.AdminUpdate(ctx, id, staff, UpdateInput{Status: &resolved})
	require.NoError(t, err)
	high := "HIGH"
	_, err = s.AdminUpdate(ctx, id, staff, UpdateInput{Priority: &high})
	require.NoError(t, err)

	require.Len(t, spy.staff, 2)
	assert.Equal(t, FeedTicketCreated, spy.staff[0].Kind)
	assert.Equal(t, FeedCustomerMessage, spy.staff[1].Kind)
	for _, ev := range spy.staff {
		assert.Equal(t, id.String(), ev.TicketID)
		assert.Equal(t, org.String(), ev.OrganizationID)
		assert.Equal(t, "Site en panne", ev.Subject)
		assert.Equal(t, "URGENT", ev.Priority)
		assert.Equal(t, "awa@ex.sn", ev.AuthorEmail)
	}
	assert.Equal(t, "OPEN", spy.staff[1].Status)

	var kinds []string
	for i, ev := range spy.org {
		assert.Equal(t, "support:org:"+org.String(), spy.orgChans[i])
		assert.Equal(t, "Site en panne", ev.Subject)
		kinds = append(kinds, ev.Kind)
	}
	assert.Equal(t, []string{OrgAgentMessage, OrgStatus}, kinds, "ni la note, ni la priorité")
	assert.Equal(t, "RESOLVED", spy.org[1].Status)
}
