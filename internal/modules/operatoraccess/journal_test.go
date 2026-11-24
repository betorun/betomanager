package operatoraccess

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/core/opjournal"
)

func journalServer(j *JournalImporter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST(opjournal.Path, j.Journal)
	return r
}

func post(r http.Handler, body []byte, headers map[string]string) int {
	req := httptest.NewRequest(http.MethodPost, opjournal.Path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// Refus avant toute écriture (DB nil : aucune ne doit être tentée).
func TestJournalRefusals(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	body, _ := opjournal.Encode(opjournal.Batch{Entries: []opjournal.Entry{{UID: uuid.New(), At: now, Actor: "cli", Kind: "rescue.account.added"}}})

	if code := post(journalServer(nil), body, opjournal.Sign(priv, now, body)); code != http.StatusNotFound {
		t.Fatalf("sans clé configurée : %d", code)
	}
	r := journalServer(&JournalImporter{Keys: map[string]ed25519.PublicKey{opjournal.KeyID(pub): pub}})
	if code := post(r, body, nil); code != http.StatusUnauthorized {
		t.Fatalf("sans signature : %d", code)
	}
	if code := post(r, body, opjournal.Sign(stranger, now, body)); code != http.StatusUnauthorized {
		t.Fatalf("clé inconnue : %d", code)
	}
	if code := post(r, body, opjournal.Sign(priv, now.Add(-10*time.Minute), body)); code != http.StatusUnauthorized {
		t.Fatalf("signature ancienne : %d", code)
	}
	for name, b := range map[string]opjournal.Batch{
		"vide":          {},
		"sans uid":      {Entries: []opjournal.Entry{{At: now, Actor: "cli", Kind: "x"}}},
		"type invalide": {Entries: []opjournal.Entry{{UID: uuid.New(), At: now, Actor: "cli", Kind: "DROP TABLE"}}},
		"détail géant":  {Entries: []opjournal.Entry{{UID: uuid.New(), At: now, Actor: "cli", Kind: "x", Detail: map[string]any{"x": string(make([]byte, 5000))}}}},
	} {
		raw, _ := opjournal.Encode(b)
		if code := post(r, raw, opjournal.Sign(priv, now, raw)); code != http.StatusBadRequest {
			t.Errorf("%s : %d", name, code)
		}
	}
}

func TestAuditEntry(t *testing.T) {
	user, inv := uuid.New(), uuid.New()
	at := time.Date(2030, 3, 1, 9, 0, 0, 0, time.FixedZone("WAT", 3600))
	e := AuditEntry(opjournal.Entry{UID: uuid.New(), At: at, Actor: user.String(), Kind: "investigation.opened",
		InvestigationID: &inv, Detail: map[string]any{"reason": "ticket 42", "source": "falsifié"}}, "abcd")
	if e.Principal != "user:"+user.String() || e.Action != "operator.investigation.opened" || e.Decision != audit.DecisionAllow ||
		e.Resource != "platform/operator/investigations/"+inv.String() || !e.At.Equal(at) || e.At.Location() != time.UTC ||
		e.Metadata["reason"] != "ticket 42" || e.Metadata["source"] != "operator" || e.Metadata["operator_key"] != "abcd" {
		t.Fatalf("%+v", e)
	}
	e = AuditEntry(opjournal.Entry{UID: uuid.New(), At: at, Actor: "rescue:awa@betorun.test", Kind: "rescue.login.failed"}, "abcd")
	if e.Principal != "operator:rescue:awa@betorun.test" || e.Decision != audit.DecisionDeny || e.Resource != "platform/operator" {
		t.Fatalf("%+v", e)
	}
}
