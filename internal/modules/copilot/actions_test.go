package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// memProposals : ProposalStore en mémoire.
type memProposals struct {
	mu      sync.Mutex
	items   map[string]*service.Proposal
	claimed map[string]bool
}

func newMem() *memProposals {
	return &memProposals{items: map[string]*service.Proposal{}, claimed: map[string]bool{}}
}
func (m *memProposals) Save(_ context.Context, p *service.Proposal, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.items[p.Token] = &cp
	return nil
}
func (m *memProposals) Load(_ context.Context, token, user string) (*service.Proposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[token]
	if !ok || p.UserID != user {
		return nil, service.ErrProposalNotFound
	}
	cp := *p
	return &cp, nil
}
func (m *memProposals) Claim(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimed[token] {
		return service.ErrProposalHandled
	}
	m.claimed[token] = true
	return nil
}
func (m *memProposals) Drop(_ context.Context, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, token)
}

type fakeMinter struct{ scopes []string }

func (f *fakeMinter) MintDelegated(d sts.Delegation) (string, time.Time, error) {
	f.scopes = append(f.scopes, d.Scope)
	return "tok-" + d.Scope, time.Now().Add(d.TTL), nil
}

// api : enregistre les requêtes ; /w/missing → 404.
type api struct {
	mu   sync.Mutex
	reqs []string
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Idempotency-Key"))
	a.mu.Unlock()
	if strings.Contains(r.URL.Path, "missing") {
		w.WriteHeader(404)
		return
	}
	if r.Method == http.MethodGet {
		fmt.Fprint(w, `{"name":"web"}`)
		return
	}
	fmt.Fprint(w, `{"status":"ok"}`)
}

const actionSpec = `{"paths":{
 "/w/{id}":{"get":{"summary":"W","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/status":{"get":{"summary":"S","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/events":{"get":{"summary":"E","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/logs":{"get":{"summary":"L","parameters":[{"name":"id","in":"path","required":true}]}},
 "/workloads/{id}/stop":{"post":{"summary":"Stop","parameters":[{"name":"id","in":"path","required":true}]}},
 "/workloads/{id}/start":{"post":{"summary":"Start","parameters":[{"name":"id","in":"path","required":true}]}},
 "/workloads/{id}":{"get":{"summary":"Get","parameters":[{"name":"id","in":"path","required":true}]}},
 "/workloads/{id}/variables":{"patch":{"summary":"Vars","parameters":[{"name":"id","in":"path","required":true}],
   "requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Vars"}}}}}}},
 "components":{"schemas":{"Vars":{"type":"object","properties":{"env":{"type":"object"},"secrets":{"type":"object"}}}}}}`

func actionService(t *testing.T) (*service.Service, *api, *fakeMinter) {
	t.Helper()
	a, m := &api{}, &fakeMinter{}
	s := &service.Service{LLM: llmtest.New("p"), STS: m, Proposals: newMem(), Logger: zap.NewNop().Sugar()}
	err := s.Bind(a, []tools.Route{
		{Name: tools.ToolGetWorkload, Method: "GET", Path: "/workloads/:id"},
		{Name: tools.ToolGetWorkloadStatus, Method: "GET", Path: "/w/:id/status"},
		{Name: tools.ToolGetWorkloadEvents, Method: "GET", Path: "/w/:id/events"},
		{Name: tools.ToolGetWorkloadLogs, Method: "GET", Path: "/w/:id/logs"},
		{Name: "stop_workload", Method: "POST", Path: "/workloads/:id/stop", Write: true, Risk: tools.RiskMedium, Undo: "start_workload"},
		{Name: "start_workload", Method: "POST", Path: "/workloads/:id/start", Write: true, Risk: tools.RiskLow, Undo: "stop_workload"},
		{Name: "update_workload_variables", Method: "PATCH", Path: "/workloads/:id/variables", Write: true, Risk: tools.RiskMedium, Omit: []string{"secrets"}},
	}, []byte(actionSpec), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, a, m
}

func runner(s *service.Service, user string, views *[]service.View) *service.BoundRunner {
	return &service.BoundRunner{Cat: s.Catalog, Inv: tools.Invoker{Handler: s.Handler, Token: "tok-read"},
		Svc: s, UserID: user, ConversationID: "conv", OnProposal: func(v service.View) { *views = append(*views, v) }}
}

func call(t *testing.T, s *service.Service, method, token string, user, session uuid.UUID) (*httptest.ResponseRecorder, ActionResult) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/copilot/actions/"+token+"/"+method, nil)
	c.Params = gin.Params{{Key: "token", Value: token}}
	c.Set("authMethod", "bearer")
	c.Set("internalUserID", user.String())
	c.Set("sessionID", session.String())
	h := NewController(s)
	if method == "approve" {
		h.Approve(c)
	} else {
		h.Reject(c)
	}
	var out ActionResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestProposeNeverExecutesAndOneCardPerMessage(t *testing.T) {
	s, a, _ := actionService(t)
	var views []service.View
	r := runner(s, uuid.NewString(), &views)
	res := r.Run(context.Background(), "stop_workload", json.RawMessage(`{"id":"w1"}`))
	if res.IsError || !strings.Contains(res.Content, "awaiting_validation") {
		t.Fatalf("proposition : %+v", res)
	}
	for _, req := range a.reqs {
		if strings.HasPrefix(req, "POST") {
			t.Fatalf("une écriture a été jouée sans validation : %s", req)
		}
	}
	if len(views) != 1 || views[0].Target != "web" || views[0].Risk != tools.RiskMedium || len(views[0].Token) != 64 {
		t.Fatalf("carte : %+v", views)
	}
	if res := r.Run(context.Background(), "start_workload", json.RawMessage(`{"id":"w1"}`)); !res.IsError {
		t.Fatal("deuxième carte dans le même message acceptée")
	}
	// Cible introuvable : pas de carte.
	r2 := runner(s, uuid.NewString(), &views)
	if res := r2.Run(context.Background(), "stop_workload", json.RawMessage(`{"id":"missing"}`)); !res.IsError {
		t.Fatal("carte créée pour une ressource introuvable")
	}
	// Le champ secrets n'est pas un argument : refusé.
	r3 := runner(s, uuid.NewString(), &views)
	if res := r3.Run(context.Background(), "update_workload_variables", json.RawMessage(`{"id":"w1","secrets":{"K":"v"}}`)); !res.IsError {
		t.Fatal("secret accepté comme argument du modèle")
	}
	// Un appel direct d'écriture par Call est refusé.
	if res := s.Catalog.Call(context.Background(), tools.Invoker{Handler: a}, "stop_workload", json.RawMessage(`{"id":"w1"}`)); !res.IsError {
		t.Fatal("Call a exécuté une écriture")
	}
}

func TestApproveOnceWithWriteTokenThenUndo(t *testing.T) {
	s, a, m := actionService(t)
	user, session := uuid.New(), uuid.New()
	var views []service.View
	runner(s, user.String(), &views).Run(context.Background(), "stop_workload", json.RawMessage(`{"id":"w1"}`))
	token := views[0].Token

	// Une autre personne ne voit pas la carte.
	if w, _ := call(t, s, "approve", token, uuid.New(), uuid.New()); w.Code != http.StatusGone {
		t.Fatalf("carte d'autrui : %d", w.Code)
	}

	w, out := call(t, s, "approve", token, user, session)
	if w.Code != 200 || out.Status != "done" {
		t.Fatalf("validation : %d %s", w.Code, w.Body)
	}
	last := a.reqs[len(a.reqs)-1]
	if last != "POST /workloads/w1/stop Bearer tok-write "+token {
		t.Fatalf("requête jouée : %q", last)
	}
	if m.scopes[len(m.scopes)-1] != sts.ScopeWrite {
		t.Fatal("jeton d'écriture attendu à la validation")
	}
	if string(out.Result) != `{"status":"ok"}` {
		t.Fatalf("résultat de la route : %s", out.Result)
	}
	if out.Undo == nil || out.Undo.Tool != "start_workload" || !out.Undo.Undo {
		t.Fatalf("carte d'annulation : %+v", out.Undo)
	}
	// Deuxième clic : la carte est consommée.
	if w, _ := call(t, s, "approve", token, user, session); w.Code != http.StatusGone && w.Code != http.StatusConflict {
		t.Fatalf("double validation : %d", w.Code)
	}
	// Annuler = valider la carte inverse.
	w, out = call(t, s, "approve", out.Undo.Token, user, session)
	if out.Status != "done" || !strings.HasPrefix(a.reqs[len(a.reqs)-1], "POST /workloads/w1/start") {
		t.Fatalf("annulation : %d %s", w.Code, w.Body)
	}
}

func TestRejectDoesNothing(t *testing.T) {
	s, a, _ := actionService(t)
	user, session := uuid.New(), uuid.New()
	var views []service.View
	runner(s, user.String(), &views).Run(context.Background(), "stop_workload", json.RawMessage(`{"id":"w1"}`))
	before := len(a.reqs)
	if w, out := call(t, s, "reject", views[0].Token, user, session); w.Code != 200 || out.Status != "rejected" {
		t.Fatalf("refus : %d %s", w.Code, w.Body)
	}
	if len(a.reqs) != before {
		t.Fatal("un refus a appelé l'API")
	}
	if w, _ := call(t, s, "approve", views[0].Token, user, session); w.Code == 200 {
		t.Fatal("carte refusée puis validée")
	}
}
