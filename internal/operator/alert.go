package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// WebhookAlerter : prévient l'équipe par un webhook entrant (Slack, Google
// Chat, Mattermost… : tous acceptent {"text": "…"}). Le service de
// messagerie doit être hors du serveur 1 : l'alerte part même quand
// celui-ci est en panne.
type WebhookAlerter struct {
	// URL : secret (elle suffit pour publier), jamais journalisée.
	URL    string
	HTTP   *http.Client
	Logger *slog.Logger
}

// Alert : un essai, puis un second après 2 s ; l'échec est journalisé sans
// l'URL.
func (a *WebhookAlerter) Alert(ctx context.Context, text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	var status int
send:
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				break send
			case <-time.After(2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
		if err != nil {
			break
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			status = 0
			continue
		}
		resp.Body.Close()
		if status = resp.StatusCode; status < 300 {
			return
		}
	}
	if a.Logger != nil {
		a.Logger.Error("operator alert not delivered", "status", status)
	}
}

// LogAlerter : développement seulement (OPERATOR_ALERT_LOG_ONLY) ; l'alerte
// va dans le journal du processus.
type LogAlerter struct{ Logger *slog.Logger }

// Alert : rescue.Alerter.
func (a LogAlerter) Alert(_ context.Context, text string) {
	a.Logger.Warn("operator alert (log only, development)", "text", text)
}
