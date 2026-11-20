package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

type plainSealer struct{}

func (plainSealer) Seal(p []byte, _ string) ([]byte, error) { return append([]byte{}, p...), nil }
func (plainSealer) Open(p []byte, _ string) ([]byte, error) { return append([]byte{}, p...), nil }

func TestValidateWebhookURL(t *testing.T) {
	for _, ok := range []string{"https://hooks.slack.com/services/T0/B0/xyz", "https://example.com/hook?x=1"} {
		_, err := validateWebhookURL(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"http://example.com/h", "https://localhost/h", "https://127.0.0.1/h", "https://10.0.0.5/h",
		"https://169.254.169.254/latest", "https://100.64.1.1/h", "https://user:pw@example.com/h", "https://intranet/h",
		"https://api.svc.local/h", "ftp://example.com", "https://[::1]/h"} {
		_, err := validateWebhookURL(bad)
		assert.ErrorIs(t, err, ErrWebhookURL, bad)
	}
}

func TestHint(t *testing.T) {
	u, _ := validateWebhookURL("https://hooks.slack.com/services/T0/B0/abcdWXYZ")
	assert.Equal(t, "hooks.slack.com/…WXYZ", hint(u), "jamais l'URL complète (secrète pour Slack)")
}

func TestPost_SignedJSONAndSlack(t *testing.T) {
	var got struct {
		body           []byte
		sig, ts, ctype string
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.sig, got.ts, got.ctype = r.Header.Get("X-Betorun-Signature"), r.Header.Get("X-Betorun-Timestamp"), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := &WebhookService{Sealer: plainSealer{}, Client: srv.Client()}
	ev := WebhookEvent{Type: "disk.100", ProjectID: uuid.New(), Project: "boutique", Title: "Disque plein", Text: "a <b> & c",
		Link: "https://console/x", SentAt: time.Unix(1_800_000_000, 0)}

	row := webhookRow{Webhook: Webhook{ID: uuid.New(), Kind: WebhookJSON}, URLSealed: []byte(srv.URL), SecretSealed: []byte("whsec_test")}
	require.Equal(t, "ok", s.post(context.Background(), row, ev))
	m := hmac.New(sha256.New, []byte("whsec_test"))
	m.Write([]byte(got.ts + "." + string(got.body)))
	assert.Equal(t, "sha256="+hex.EncodeToString(m.Sum(nil)), got.sig)
	assert.Equal(t, "1800000000", got.ts)
	var back WebhookEvent
	require.NoError(t, json.Unmarshal(got.body, &back))
	assert.Equal(t, "disk.100", back.Type)

	row.Kind, row.SecretSealed = WebhookSlack, nil
	require.Equal(t, "ok", s.post(context.Background(), row, ev))
	var slack map[string]string
	require.NoError(t, json.Unmarshal(got.body, &slack))
	assert.Equal(t, "*Disque plein*\na &lt;b&gt; &amp; c\n<https://console/x|boutique>", slack["text"])
	assert.Empty(t, got.sig, "Slack : pas de signature")
}

func TestPost_PrivateAddressRefusedAtDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	s := &WebhookService{Sealer: plainSealer{}} // client public par défaut
	row := webhookRow{Webhook: Webhook{ID: uuid.New(), Kind: WebhookSlack}, URLSealed: []byte(srv.URL)}
	assert.Equal(t, "private address refused", s.post(context.Background(), row, WebhookEvent{}))
}

func TestPost_HTTPError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	s := &WebhookService{Sealer: plainSealer{}, Client: srv.Client()}
	row := webhookRow{Webhook: Webhook{ID: uuid.New(), Kind: WebhookSlack}, URLSealed: []byte(srv.URL)}
	assert.Equal(t, "HTTP 404", s.post(context.Background(), row, WebhookEvent{}))
}

func TestDeliver_SendsToWebhooksOnce(t *testing.T) {
	n, m := newNotifier()
	rec := &recordingHooks{}
	n.Webhooks = rec
	n.DiskUsage(context.Background(), uuid.New(), uuid.New(), 100, "workload", "api", 1<<30, 1<<30)
	assert.Len(t, m.sent, 2, "deux destinataires")
	require.Len(t, rec.kinds, 1, "un seul envoi aux webhooks")
	assert.Equal(t, "disk.100", rec.kinds[0])
}

type recordingHooks struct{ kinds []string }

func (r *recordingHooks) Send(_ context.Context, _ uuid.UUID, kind string, _ uuid.UUID, _, _ string, texts map[i18n.Locale][2]string) {
	r.kinds = append(r.kinds, kind)
}
