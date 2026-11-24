package operator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

type fakeAuth map[string]Identity

func (f fakeAuth) Identify(_ context.Context, token string) (Identity, error) {
	if token == "down" {
		return Identity{}, ErrAPIUnavailable
	}
	id, ok := f[token]
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

var (
	admin   = Identity{UserID: uuid.New(), Email: "admin@betorun.test", Capabilities: Capabilities{Infrastructure: true, Support: true}}
	support = Identity{UserID: uuid.New(), Email: "support@betorun.test", Capabilities: Capabilities{Support: true}}
)

func server(t *testing.T, model *llmtest.Provider) (*httptest.Server, *memStore) {
	t.Helper()
	iv, m := setup(model)
	s := &Server{Store: m, Auth: fakeAuth{"a": admin, "s": support}, Investigator: iv, Infrastructure: clusters,
		Model: "claude-opus-5-5", Origins: []string{"http://localhost:3001"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func call(t *testing.T, srv *httptest.Server, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", "http://localhost:3001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestServerFlow(t *testing.T) {
	model := llmtest.New("claude",
		llmtest.Call("c1", "k8s_pods", `{"cluster":"prod-1","problems_only":true}`),
		llmtest.Say("web-1 redémarre en boucle."))
	srv, m := server(t, model)

	if r, _ := call(t, srv, "GET", "/v1/me", "", ""); r.StatusCode != 401 {
		t.Fatalf("sans jeton : %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "GET", "/v1/me", "down", ""); r.StatusCode != 503 {
		t.Fatalf("API absente : %d", r.StatusCode)
	}
	r, body := call(t, srv, "GET", "/v1/me", "a", "")
	if r.StatusCode != 200 || !strings.Contains(body, `"infrastructure":true`) || r.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3001" {
		t.Fatalf("me : %d %s %v", r.StatusCode, body, r.Header)
	}

	if r, _ := call(t, srv, "POST", "/v1/investigations", "a", `{"subject_kind":"secret"}`); r.StatusCode != 400 {
		t.Fatalf("sujet inconnu : %d", r.StatusCode)
	}
	r, body = call(t, srv, "POST", "/v1/investigations", "a", `{"subject_kind":"cluster","subject_ref":"prod-1","reason":""}`)
	if r.StatusCode != 201 {
		t.Fatalf("création : %d %s", r.StatusCode, body)
	}
	var inv InvestigationView
	_ = json.Unmarshal([]byte(body), &inv)
	if m.inv.ActorEmail != admin.Email || m.inv.Model != "claude-opus-5-5" {
		t.Fatalf("dossier = %+v", m.inv)
	}
	path := "/v1/investigations/" + inv.ID.String()

	// Une autre personne lit le dossier mais ne le fait pas avancer.
	if r, _ := call(t, srv, "GET", path, "s", ""); r.StatusCode != 200 {
		t.Fatalf("lecture par l'équipe : %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "POST", path+"/messages", "s", `{"message":"x"}`); r.StatusCode != 403 {
		t.Fatalf("question d'un autre : %d", r.StatusCode)
	}

	r, body = call(t, srv, "POST", path+"/messages", "a", `{"message":"Pourquoi km-web est en panne ?"}`)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("flux : %d %s", r.StatusCode, body)
	}
	for _, want := range []string{"event: tool_start", `"name":"k8s_pods"`, "event: text", "event: done", `"stop":"complete"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("%q absent du flux :\n%s", want, body)
		}
	}
	if got := model.Requests()[0].Tools; len(got) == 0 {
		t.Fatal("outils d'infrastructure absents pour un administrateur")
	}

	r, body = call(t, srv, "GET", path, "a", "")
	if r.StatusCode != 200 || !strings.Contains(body, `"tool":"k8s_pods"`) {
		t.Fatalf("dossier : %s", body)
	}
	if r, _ := call(t, srv, "POST", path+"/close", "a", `{"status":"open"}`); r.StatusCode != 400 {
		t.Fatalf("statut invalide : %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "POST", path+"/close", "a", `{"status":"concluded","conclusion":"Limite mémoire."}`); r.StatusCode != 204 {
		t.Fatalf("clôture : %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "POST", path+"/messages", "a", `{"message":"x"}`); r.StatusCode != 409 {
		t.Fatalf("question sur dossier clos : %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "GET", "/v1/investigations/"+uuid.NewString(), "a", ""); r.StatusCode != 404 {
		t.Fatalf("inconnu : %d", r.StatusCode)
	}
}

// Une personne du support n'a pas les outils du chemin de secours.
func TestSupportHasNoInfrastructureTools(t *testing.T) {
	model := llmtest.New("claude", llmtest.Say("Je n'ai pas accès aux clusters."))
	srv, m := server(t, model)
	m.inv = store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectTicket, ActorUserID: support.UserID.String(), Status: store.StatusOpen}
	r, body := call(t, srv, "POST", "/v1/investigations/"+m.inv.ID.String()+"/messages", "s", `{"message":"État du cluster ?"}`)
	if r.StatusCode != 200 || !strings.Contains(body, "event: done") {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
	if len(model.Requests()[0].Tools) != 0 {
		t.Fatal("outils d'infrastructure proposés au support")
	}
}

func TestCORS(t *testing.T) {
	srv, _ := server(t, llmtest.New("claude"))
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/v1/investigations", nil)
	req.Header.Set("Origin", "http://localhost:3001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("pré-vol : %d %v", resp.StatusCode, resp.Header)
	}
	req.Header.Set("Origin", "https://evil.test")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("origine inconnue autorisée")
	}
}

func TestHealth(t *testing.T) {
	srv, _ := server(t, llmtest.New("claude"))
	r, body := call(t, srv, "GET", "/healthz", "", "")
	if r.StatusCode != 200 || !strings.Contains(body, `"agent":true`) {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
}

func TestAPIAuth(t *testing.T) {
	user := uuid.New()
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/kmanager/v1/admin/operator/me" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer good":
			_ = json.NewEncoder(w).Encode(map[string]any{"user_id": user, "email": "a@b.test", "capabilities": map[string]bool{"support": true}})
		case "Bearer broken":
			w.WriteHeader(500)
		default:
			w.WriteHeader(403)
		}
	}))
	defer api.Close()
	now := time.Unix(0, 0)
	a := &APIAuth{BaseURL: api.URL + "/kmanager/v1", Now: func() time.Time { return now }}
	id, err := a.Identify(context.Background(), "good")
	if err != nil || id.UserID != user || !id.Capabilities.Support || id.Token != "good" {
		t.Fatalf("%+v %v", id, err)
	}
	_, _ = a.Identify(context.Background(), "good")
	if calls != 1 {
		t.Fatalf("cache ignoré : %d appels", calls)
	}
	now = now.Add(2 * time.Minute)
	_, _ = a.Identify(context.Background(), "good")
	if calls != 2 {
		t.Fatal("cache non expiré")
	}
	if _, err := a.Identify(context.Background(), "bad"); err != ErrUnauthenticated {
		t.Fatalf("refusé : %v", err)
	}
	if _, err := a.Identify(context.Background(), "broken"); err == nil || err == ErrUnauthenticated {
		t.Fatalf("API en erreur : %v", err)
	}
	if _, err := (&APIAuth{}).Identify(context.Background(), "good"); err != ErrAPIUnavailable {
		t.Fatalf("sans API : %v", err)
	}
}
