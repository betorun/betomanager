package alerts

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

func TestUnsubscribeToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	user := uuid.New()
	tok := UnsubscribeToken(key, user, CategoryDisk)
	got, cat, err := ParseUnsubscribeToken(key, tok)
	require.NoError(t, err)
	assert.Equal(t, user, got)
	assert.Equal(t, CategoryDisk, cat)

	_, _, err = ParseUnsubscribeToken([]byte("autre-cle-autre-cle-autre-cle-xx"), tok)
	assert.ErrorIs(t, err, ErrInvalidToken, "autre clé")
	forged := UnsubscribeToken(key, user, CategoryDisk)
	forged = strings.Replace(forged, forged[:4], "AAAA", 1)
	_, _, err = ParseUnsubscribeToken(key, forged)
	assert.ErrorIs(t, err, ErrInvalidToken, "contenu modifié")
	_, _, err = ParseUnsubscribeToken(nil, tok)
	assert.ErrorIs(t, err, ErrInvalidToken, "sans clé")
}

func TestCategoryOf(t *testing.T) {
	assert.Equal(t, CategoryWorkloads, categoryOf(KindWorkloadIncident))
	assert.Equal(t, CategoryBuilds, categoryOf(KindBuildFailed))
	assert.Equal(t, CategoryDisk, categoryOf("disk.120"))
	assert.Equal(t, CategoryAPIKeys, categoryOf(KindAPIKeyCreated))
	assert.Equal(t, "", categoryOf(KindBalance), "facturation : ne se coupe pas")
}

// Personne qui a coupé la catégorie : rien ; les autres : lien de
// désabonnement, en-tête « un clic » et langue choisie.
func TestDeliver_MutedLocaleAndUnsubscribe(t *testing.T) {
	m := &fakeMail{}
	awa, moussa := uuid.New(), uuid.New()
	store := &fakeStore{claimed: map[string]bool{}, owners: []Recipient{
		{ID: awa, Email: "awa@example.com", FullName: "Awa", Locale: "en"},
		{ID: moussa, Email: "moussa@example.com"}},
		muted: map[string]map[uuid.UUID]bool{CategoryWorkloads: {moussa: true}}}
	n := &Notifier{Store: store, Mail: m, ConsoleURL: "https://console.example.com", APIURL: "https://api.example.com/kmanager/v1",
		UnsubscribeKey: []byte("0123456789abcdef0123456789abcdef"), Sync: true}
	n.WorkloadIncident(i18n.With(context.Background(), i18n.FR), uuid.New(), uuid.New(), "api", utils.WorkloadOOMKilled)

	require.Len(t, m.sent, 1, "moussa a coupé les alertes de conteneurs")
	msg := m.sent[0]
	assert.Equal(t, "awa@example.com", msg.To)
	assert.Equal(t, `"api" ran out of memory`, msg.Subject, "langue choisie par awa")
	assert.Contains(t, msg.Text, "Stop receiving this kind of alert : https://console.example.com/unsubscribe?token=")
	assert.True(t, strings.HasPrefix(msg.Unsubscribe, "https://api.example.com/kmanager/v1/alerts/unsubscribe?token="))
}

func TestBillingAlerts_NoUnsubscribe(t *testing.T) {
	n, m := newNotifier()
	n.UnsubscribeKey = []byte("0123456789abcdef0123456789abcdef")
	n.BalanceAlert(context.Background(), uuid.New(), "empty", 0, nil, "XAF")
	require.NotEmpty(t, m.sent)
	assert.NotContains(t, m.sent[0].Text, "unsubscribe")
	assert.Empty(t, m.sent[0].Unsubscribe)
}
