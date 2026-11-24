package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

type emailOutbox struct{ sent []mail.Message }

func (f *emailOutbox) Send(_ context.Context, m mail.Message) error {
	f.sent = append(f.sent, m)
	return nil
}

func TestValidEmail(t *testing.T) {
	for in, want := range map[string]string{"Ops@Entreprise.com": "ops@entreprise.com", " awa.diallo@gmail.com ": "awa.diallo@gmail.com"} {
		got, err := validEmail(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	for _, bad := range []string{"", "pas une adresse", "Awa <awa@gmail.com>", "a@b", "a@gmail.com, b@gmail.com", "@gmail.com"} {
		_, err := validEmail(bad)
		assert.ErrorIs(t, err, ErrEmailAddress, bad)
	}
	assert.Equal(t, "o…@entreprise.com", maskEmail("ops@entreprise.com"))
}

func TestEmailTokens(t *testing.T) {
	key := []byte("clé de test")
	id := uuid.New()
	now := time.Unix(1_800_000_000, 0)

	confirm := emailToken(key, "confirm", id, now.Add(EmailConfirmTTL))
	got, err := parseEmailToken(key, "confirm", confirm, now)
	require.NoError(t, err)
	assert.Equal(t, id, got)
	_, err = parseEmailToken(key, "confirm", confirm, now.Add(EmailConfirmTTL+time.Minute))
	assert.ErrorIs(t, err, ErrEmailLink, "lien de confirmation expiré")
	_, err = parseEmailToken(key, "remove", confirm, now)
	assert.ErrorIs(t, err, ErrEmailLink, "un jeton ne sert qu'à son usage")
	_, err = parseEmailToken([]byte("autre clé"), "confirm", confirm, now)
	assert.ErrorIs(t, err, ErrEmailLink)

	remove := emailToken(key, "remove", id, time.Time{})
	got, err = parseEmailToken(key, "remove", remove, now.Add(10*365*24*time.Hour))
	require.NoError(t, err, "le lien de retrait n'expire pas")
	assert.Equal(t, id, got)
	_, err = parseEmailToken(key, "remove", remove+"x", now)
	assert.ErrorIs(t, err, ErrEmailLink)
}

func TestSendEmailAlert_WithRemoveLinks(t *testing.T) {
	m := &emailOutbox{}
	s := &WebhookService{Sealer: plainSealer{}, Mail: m, Key: []byte("k"), ConsoleURL: "https://console.test/", APIURL: "https://api.test/kmanager/v1"}
	row := webhookRow{Webhook: Webhook{ID: uuid.New(), Kind: WebhookEmail, Locale: "fr"}, URLSealed: []byte("mailto:ops@entreprise.com")}
	ev := WebhookEvent{Project: "boutique", Title: "Disque plein", Text: "Le disque <db> est plein.", Link: "https://console.test/x"}
	require.Equal(t, "ok", s.post(context.Background(), row, ev))
	require.Len(t, m.sent, 1)
	msg := m.sent[0]
	assert.Equal(t, "ops@entreprise.com", msg.To)
	assert.Equal(t, "Disque plein", msg.Subject)
	assert.Contains(t, msg.Text, "« boutique »")
	assert.Contains(t, msg.Text, "https://console.test/alert-email?remove=")
	assert.True(t, strings.HasPrefix(msg.Unsubscribe, "https://api.test/kmanager/v1/alerts/email/remove?token="), "retrait en un clic")
	assert.Contains(t, msg.HTML, "&lt;db&gt;", "texte échappé dans le HTML")

	s.Mail = nil
	assert.Equal(t, "error", s.post(context.Background(), row, ev), "sans envoi d'e-mails, rien ne part")
}
