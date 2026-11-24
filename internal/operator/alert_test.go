package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWebhookAlerter(t *testing.T) {
	var hits atomic.Int32
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // premier essai refusé : second essai
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = body["text"]
	}))
	defer srv.Close()
	(&WebhookAlerter{URL: srv.URL, Logger: quietLogger()}).Alert(context.Background(), "connexion de SECOURS")
	if hits.Load() != 2 || got != "connexion de SECOURS" {
		t.Fatalf("essais %d, texte %q", hits.Load(), got)
	}
}
