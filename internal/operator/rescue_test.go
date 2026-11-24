package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/rescue"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

type fakeRescue struct {
	acc      store.RescueAccount
	sessions map[string]bool
	attempts int
}

func (f *fakeRescue) Login(_ context.Context, email, password, code, reason, _ string) (string, time.Time, store.RescueAccount, error) {
	if len(reason) < 10 {
		return "", time.Time{}, f.acc, rescue.ErrReason
	}
	f.attempts++
	if f.attempts > 3 {
		return "", time.Time{}, f.acc, rescue.ErrLocked
	}
	if email != f.acc.Email || password != "bon mot de passe long" || code != "123456" {
		return "", time.Time{}, f.acc, rescue.ErrBadCredentials
	}
	tok := rescue.TokenPrefix + uuid.NewString()
	f.sessions[tok] = true
	return tok, time.Now().Add(rescue.SessionTTL), f.acc, nil
}

func (f *fakeRescue) Session(_ context.Context, token string) (store.RescueAccount, error) {
	if !f.sessions[token] {
		return store.RescueAccount{}, rescue.ErrBadCredentials
	}
	return f.acc, nil
}

func (f *fakeRescue) Logout(_ context.Context, token, _ string) error {
	delete(f.sessions, token)
	return nil
}

func TestRescueFlow(t *testing.T) {
	var seen []*http.Request
	model := llmtest.New("claude", llmtest.Say("Le nœud n2 ne répond plus."))
	iv, m := setup(model)
	fr := &fakeRescue{acc: store.RescueAccount{ID: uuid.New(), Email: "awa@betorun.test"}, sessions: map[string]bool{}}
	s := &Server{Store: m, Auth: fakeAuth{"a": admin}, Rescue: fr, Investigator: iv, Infrastructure: clusters,
		API: &APITools{BaseURL: fakeAdminAPI(t, &seen).URL + "/kmanager/v1"}, Model: "claude-opus-5-5", Logger: quietLogger()}
	h := httptest.NewServer(s.Handler())
	defer h.Close()

	if r, body := call(t, h, "POST", "/v1/rescue/login", "", `{"email":"awa@betorun.test","password":"bon mot de passe long","code":"123456"}`); r.StatusCode != 400 || !strings.Contains(body, "REASON_REQUIRED") {
		t.Fatalf("sans motif : %d %s", r.StatusCode, body)
	}
	if r, _ := call(t, h, "POST", "/v1/rescue/login", "", `{"email":"awa@betorun.test","password":"faux","code":"123456","reason":"API en panne depuis 10 h"}`); r.StatusCode != 401 {
		t.Fatalf("refus : %d", r.StatusCode)
	}
	r, body := call(t, h, "POST", "/v1/rescue/login", "", `{"email":"awa@betorun.test","password":"bon mot de passe long","code":"123456","reason":"API en panne depuis 10 h"}`)
	if r.StatusCode != 200 {
		t.Fatalf("connexion : %d %s", r.StatusCode, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(body), &login)

	r, body = call(t, h, "GET", "/v1/me", login.Token, "")
	if r.StatusCode != 200 || !strings.Contains(body, `"rescue":true`) || !strings.Contains(body, `"infrastructure":true`) || strings.Contains(body, `"support":true`) {
		t.Fatalf("me : %s", body)
	}
	r, body = call(t, h, "POST", "/v1/investigations", login.Token, `{"subject_kind":"cluster","subject_ref":"prod-1"}`)
	if r.StatusCode != 201 || m.inv.ActorUserID != "rescue:"+fr.acc.ID.String() {
		t.Fatalf("dossier : %d %s %+v", r.StatusCode, body, m.inv)
	}
	r, body = call(t, h, "POST", "/v1/investigations/"+m.inv.ID.String()+"/messages", login.Token, `{"message":"État de prod-1 ?"}`)
	if r.StatusCode != 200 || !strings.Contains(body, "event: done") {
		t.Fatalf("question : %d %s", r.StatusCode, body)
	}
	names := map[string]bool{}
	for _, d := range model.Requests()[0].Tools {
		names[d.Name] = true
	}
	if !names["k8s_pods"] || names["get_ticket"] || len(seen) != 0 {
		t.Fatalf("session de secours : outils %v, appels à l'API %d", names, len(seen))
	}

	r, body = call(t, h, "GET", "/v1/journal", login.Token, "")
	if !strings.Contains(body, `"kind":"investigation.opened"`) || !strings.Contains(body, `"uid":`) || strings.Contains(body, `"ID"`) {
		t.Fatalf("format du journal : %s", body)
	}
	for _, kind := range []string{"investigation.opened", "investigation.question"} {
		if !strings.Contains(body, kind) {
			t.Fatalf("journal sans %s : %s", kind, body)
		}
	}
	if r, _ := call(t, h, "POST", "/v1/rescue/logout", login.Token, ""); r.StatusCode != 204 {
		t.Fatalf("déconnexion : %d", r.StatusCode)
	}
	if r, _ := call(t, h, "GET", "/v1/me", login.Token, ""); r.StatusCode != 401 {
		t.Fatalf("session révoquée : %d", r.StatusCode)
	}
	if r, _ := call(t, h, "POST", "/v1/rescue/logout", "a", ""); r.StatusCode != 400 {
		t.Fatalf("déconnexion d'une session de l'API : %d", r.StatusCode)
	}
	call(t, h, "POST", "/v1/rescue/login", "", `{"email":"x","password":"y","code":"z","reason":"API en panne depuis 10 h"}`)
	if r, _ := call(t, h, "POST", "/v1/rescue/login", "", `{"email":"x","password":"y","code":"z","reason":"API en panne depuis 10 h"}`); r.StatusCode != 429 {
		t.Fatalf("verrouillage : %d", r.StatusCode)
	}
}

func TestRescueDisabled(t *testing.T) {
	srv, _ := server(t, llmtest.New("claude"))
	if r, _ := call(t, srv, "POST", "/v1/rescue/login", "", `{"email":"a","password":"b","code":"c"}`); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
	if r, _ := call(t, srv, "GET", "/v1/me", rescue.TokenPrefix+"x", ""); r.StatusCode != 401 {
		t.Fatalf("%d", r.StatusCode)
	}
}
