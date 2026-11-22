package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
)

func TestAccessChanged(t *testing.T) {
	n, m := newNotifier()
	to := domain.Recipient{Email: "kofi@example.com", FullName: "Kofi"}
	n.AccessChanged(context.Background(), to, ResourceProject, AccessGranted, "boutique", []string{"Éditeur"}, "awa@example.com")
	require.Len(t, m.sent, 1)
	msg := m.sent[0]
	assert.Equal(t, "kofi@example.com", msg.To)
	assert.Equal(t, "Vous avez accès au projet « boutique »", msg.Subject)
	assert.Contains(t, msg.Text, "par awa@example.com. Rôle : Éditeur.")
	assert.Contains(t, msg.Text, "https://console.example.com/dashboard")
	assert.NotContains(t, msg.Text, "propriétaire ou éditeur de ce projet", "pas la fin des alertes de projet")

	m.sent = nil
	n.AccessChanged(i18n.With(context.Background(), i18n.EN), to, ResourceOrganization, AccessRemoved, "Bomba", nil, "")
	require.Len(t, m.sent, 1)
	assert.Equal(t, `Your access to organization "Bomba" was removed`, m.sent[0].Subject)
	assert.Contains(t, m.sent[0].Text, `organization "Bomba". Access granted`, "sans auteur : pas de « by »")

	m.sent = nil
	n.AccessChanged(context.Background(), to, "bucket", AccessGranted, "x", nil, "")
	assert.Empty(t, m.sent)
}

func TestAccessChanged_RoleTitleTranslated(t *testing.T) {
	n, m := newNotifier()
	n.AccessChanged(i18n.With(context.Background(), i18n.EN), domain.Recipient{Email: "k@example.com"}, ResourceProject, AccessChanged,
		"boutique", []string{"Propriétaire"}, "a@example.com")
	require.Len(t, m.sent, 1)
	assert.NotContains(t, m.sent[0].Text, "Propriétaire")
}

func TestGroupMembershipChanged(t *testing.T) {
	n, m := newNotifier()
	n.GroupMembershipChanged(context.Background(), domain.Recipient{Email: "ama@example.com"}, "devs", "Bomba", true, "awa@example.com")
	n.GroupMembershipChanged(context.Background(), domain.Recipient{Email: "ama@example.com"}, "devs", "Bomba", false, "")
	require.Len(t, m.sent, 2)
	assert.Equal(t, "Vous faites partie du groupe « devs »", m.sent[0].Subject)
	assert.Contains(t, m.sent[0].Text, "groupe « devs » de l'organisation « Bomba » par awa@example.com")
	assert.Equal(t, "Vous ne faites plus partie du groupe « devs »", m.sent[1].Subject)
}
