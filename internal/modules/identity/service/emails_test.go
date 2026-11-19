package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Chaque e-mail existe dans les deux langues et se remplit entièrement.
func TestEmails_CompleteInBothLanguages(t *testing.T) {
	vars := map[string]string{"Device": "Chrome · Windows", "IP": "41.82.1.2", "When": "le 19/11/2026 à 22:00 UTC",
		"Provider": "Google", "Email": "awa@gmail.com"}
	for kind := range emailCopies {
		for _, lang := range []i18n.Locale{i18n.FR, i18n.EN} {
			msg, err := renderEmailWith(i18n.With(context.Background(), lang), kind, "awa@x.test", "Awa", "https://c/settings#sessions", vars)
			require.NoError(t, err, "%d %s", kind, lang)
			require.NotEmpty(t, msg.Subject)
			for _, part := range []string{msg.Subject, msg.Text, msg.HTML} {
				assert.NotContains(t, part, "{{", "%d %s", kind, lang)
				assert.NotContains(t, part, "<no value>", "%d %s", kind, lang)
			}
		}
	}
}

func TestNewDeviceEmail(t *testing.T) {
	ctx := i18n.With(context.Background(), i18n.FR)
	msg, err := renderEmailWith(ctx, emailNewDevice, "awa@x.test", "Awa", "https://c/settings#sessions",
		map[string]string{"Device": "<b>Firefox</b> · Linux", "IP": "41.82.1.2", "When": whenText(ctx, time.Date(2026, 11, 19, 22, 5, 0, 0, time.UTC))})
	require.NoError(t, err)
	assert.Equal(t, "Nouvelle connexion à votre compte betorun", msg.Subject)
	assert.Contains(t, msg.Text, "Firefox")
	assert.Contains(t, msg.Text, "41.82.1.2")
	assert.Contains(t, msg.Text, "le 19/11/2026 à 22:05 UTC")
	assert.NotContains(t, msg.HTML, "<b>Firefox</b>", "libellé échappé dans la version HTML")
}

func TestLinkedEmailSubject(t *testing.T) {
	msg, err := renderEmailWith(i18n.With(context.Background(), i18n.EN), emailIdentityLinked, "a@x.test", "", "https://c",
		map[string]string{"Provider": providerTitle("github"), "Email": "a@users.noreply.github.com"})
	require.NoError(t, err)
	assert.Equal(t, "GitHub account linked to your betorun account", msg.Subject)
	assert.True(t, strings.Contains(msg.Text, "Hello a,"))
}
