package operator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/action"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// memCards : cartes en mémoire, mêmes règles que la base.
type memCards struct {
	mu    sync.Mutex
	cards []store.Card
	now   time.Time
}

func (m *memCards) ProposeCard(_ context.Context, c store.Card) (store.Card, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	open := 0
	for _, e := range m.cards {
		if e.InvestigationID == c.InvestigationID && e.Status == store.CardProposed {
			if e.Kind == c.Kind && string(e.Params) == string(c.Params) {
				return e, true, nil
			}
			open++
		}
	}
	if open >= store.MaxOpenCards {
		return store.Card{}, false, store.ErrTooManyCards
	}
	c.ID, c.Status, c.ProposedAt, c.ExpiresAt = uuid.New(), store.CardProposed, m.now, m.now.Add(store.CardTTL)
	m.cards = append(m.cards, c)
	return c, false, nil
}

func (m *memCards) DecideCard(_ context.Context, inv, id uuid.UUID, approve bool, by, note string) (store.Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.cards {
		if c.ID != id || c.InvestigationID != inv {
			continue
		}
		if c.Status != store.CardProposed || !m.now.Before(c.ExpiresAt) {
			return store.Card{}, store.ErrCardDecided
		}
		c.Status = store.CardRejected
		if approve {
			c.Status = store.CardApproved
		}
		now := m.now
		c.DecidedBy, c.DecisionNote, c.DecidedAt = by, note, &now
		m.cards[i] = c
		return c, nil
	}
	return store.Card{}, store.ErrCardNotFound
}

func (m *memCards) FinishCard(_ context.Context, id uuid.UUID, ok bool, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.cards {
		if c.ID == id && c.Status == store.CardApproved {
			c.Status, c.Result = store.CardFailed, result
			if ok {
				c.Status = store.CardExecuted
			}
			m.cards[i] = c
			return nil
		}
	}
	return store.ErrCardDecided
}

func (m *memCards) SettleCard(_ context.Context, inv, id uuid.UUID, to store.CardStatus, by, note, result string) (store.Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.cards {
		if c.ID != id || c.InvestigationID != inv {
			continue
		}
		if c.Status != store.CardProposed {
			return store.Card{}, store.ErrCardDecided
		}
		c.Status, c.DecidedBy, c.DecisionNote, c.Result = to, by, note, result
		m.cards[i] = c
		return c, nil
	}
	return store.Card{}, store.ErrCardNotFound
}

func (m *memCards) ReapInterrupted(_ context.Context, after time.Duration, note string) ([]store.Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Card
	for i, c := range m.cards {
		if c.Status == store.CardApproved && c.DecidedAt != nil && c.DecidedAt.Before(m.now.Add(-after)) {
			c.Status, c.Result = store.CardFailed, note
			m.cards[i] = c
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memCards) Cards(_ context.Context, inv uuid.UUID) ([]store.Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Card
	for _, c := range m.cards {
		if c.InvestigationID == inv {
			out = append(out, store.Expire(c, m.now))
		}
	}
	return out, nil
}

// fakeProvider : une action « test.service.restart » ; risk change l'état
// observé entre la proposition et la validation.
type fakeProvider struct {
	mu       sync.Mutex
	risk     action.Risk
	executed []action.Approval
	fail     error
}

func (f *fakeProvider) Actions() []action.Spec {
	return []action.Spec{{Kind: "test.service.restart", Description: "Redémarrer un service.", Capability: action.CapInfrastructure,
		Parameters: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}},"required":["service"],"additionalProperties":false}`)}}
}

func (f *fakeProvider) Prepare(_ context.Context, _ string, raw json.RawMessage) (action.Prepared, error) {
	var p struct {
		Service string `json:"service"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Service == "" {
		return action.Prepared{}, action.ErrRefused
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := action.Prepared{Target: "s1 / " + p.Service, Summary: "Redémarrer " + p.Service + ".", Params: raw, Risk: f.risk, State: "failed"}
	if f.risk == action.RiskHigh {
		out.Confirm = "s1"
	}
	return out, nil
}

func (f *fakeProvider) Execute(_ context.Context, _ string, _ json.RawMessage, a action.Approval) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.executed = append(f.executed, a)
	return "service redémarré, actif", nil
}

func cardServer(t *testing.T, model *llmtest.Provider, p *fakeProvider) (string, func(method, path, token, body string) (int, string), *memStore, *memCards) {
	t.Helper()
	reg, err := action.NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	iv, m := setup(model)
	cards := &memCards{now: time.Now()}
	s := &Server{Store: m, Auth: fakeAuth{"a": admin, "s": support}, Investigator: iv, Actions: reg, Cards: cards,
		Model: "claude-opus-5-5", Origins: []string{"http://localhost:3001"}, Logger: quietLogger()}
	srv := httptestServer(t, s)
	m.inv = store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectCluster, ActorUserID: admin.UserID.String(), ActorEmail: admin.Email, Status: store.StatusOpen}
	do := func(method, path, token, body string) (int, string) {
		r, b := call(t, srv, method, path, token, body)
		return r.StatusCode, b
	}
	return "/v1/investigations/" + m.inv.ID.String(), do, m, cards
}

func journalKinds(m *memStore) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var k []string
	for _, e := range m.journal {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func TestCardsProposeApproveExecute(t *testing.T) {
	rationale := "k3s en échec depuis 7 redémarrages, etcd plein selon le journal du service."
	model := llmtest.New("claude",
		llmtest.Call("c1", "propose_test_service_restart", `{"service":"k3s","rationale":"court"}`),
		llmtest.Call("c2", "propose_test_service_restart", `{"service":"k3s","rationale":"`+rationale+`"}`),
		llmtest.Call("c3", "propose_test_service_restart", `{"service":"k3s","rationale":"`+rationale+`"}`),
		llmtest.Say("J'ai proposé un redémarrage de k3s : il attend votre validation."),
		llmtest.Say("Le redémarrage a été fait, k3s est actif."))
	p := &fakeProvider{risk: action.RiskMedium}
	path, do, m, cards := cardServer(t, model, p)

	code, body := do("POST", path+"/messages", "a", `{"message":"Pourquoi le serveur 1 ne répond plus ?"}`)
	if code != 200 || strings.Count(body, "event: card") != 1 || !strings.Contains(body, `"target":"s1 / k3s"`) || !strings.Contains(body, `"risk":"medium"`) {
		t.Fatalf("flux : %d\n%s", code, body)
	}
	tools := model.Requests()[0].Tools
	var def string
	for _, tl := range tools {
		if tl.Name == "propose_test_service_restart" {
			def = tl.Description + string(tl.Parameters)
		}
	}
	if !strings.Contains(def, "PROPOSE (n'exécute pas)") || !strings.Contains(def, `"rationale"`) {
		t.Fatalf("outil : %s", def)
	}
	if len(cards.cards) != 1 || cards.cards[0].Rationale != rationale || len(p.executed) != 0 {
		t.Fatalf("cartes : %+v", cards.cards)
	}
	card := cards.cards[0].ID.String()

	// Une personne sans le droit ne décide pas.
	if code, _ := do("POST", path+"/cards/"+card+"/approve", "s", `{}`); code != 403 {
		t.Fatalf("support : %d", code)
	}
	code, body = do("POST", path+"/cards/"+card+"/approve", "a", `{"note":"vu avec Awa"}`)
	if code != 200 || !strings.Contains(body, `"status":"executed"`) || !strings.Contains(body, "service redémarré") {
		t.Fatalf("validation : %d %s", code, body)
	}
	if len(p.executed) != 1 || p.executed[0].ApprovedBy != admin.Email || p.executed[0].CardID != card {
		t.Fatalf("exécution : %+v", p.executed)
	}
	if code, _ := do("POST", path+"/cards/"+card+"/approve", "a", `{}`); code != 409 {
		t.Fatalf("seconde validation : %d", code)
	}
	if k := journalKinds(m); !strings.Contains(k, "card.proposed,card.approved,card.executed") {
		t.Fatalf("journal : %s", k)
	}

	// L'issue entre dans le dossier et dans le tour suivant.
	code, _ = do("POST", path+"/messages", "a", `{"message":"Et maintenant ?"}`)
	if code != 200 {
		t.Fatal(code)
	}
	reqs := model.Requests()
	last := reqs[len(reqs)-1].Messages
	if got := last[len(last)-1].Text; !strings.Contains(got, "[Issue des actions") || !strings.Contains(got, "validée par admin@betorun.test : exécutée") {
		t.Fatalf("question suivante : %q", got)
	}

	code, body = do("GET", path, "a", "")
	if code != 200 || !strings.Contains(body, `"cards":[{`) || !strings.Contains(body, `"kind":"action"`) {
		t.Fatalf("dossier : %s", body)
	}
}

func TestCardsGuards(t *testing.T) {
	model := llmtest.New("claude", llmtest.Say("ok"))
	p := &fakeProvider{risk: action.RiskHigh}
	path, do, m, cards := cardServer(t, model, p)
	ctx := context.Background()
	prop := func(service string) string {
		pr := &proposer{reg: mustRegistry(t, p), cards: cards, inv: m.inv.ID, kinds: map[string]string{"propose_test_service_restart": "test.service.restart"}}
		res := pr.Run(ctx, "propose_test_service_restart", json.RawMessage(`{"service":"`+service+`","rationale":"cause établie par le journal du service, effet attendu : reprise"}`))
		if res.IsError {
			t.Fatalf("proposition : %s", res.Content)
		}
		return cards.cards[len(cards.cards)-1].ID.String()
	}

	// Risque élevé : le nom retapé est exigé.
	high := prop("k3s")
	if code, body := do("POST", path+"/cards/"+high+"/approve", "a", `{"confirm":"s2"}`); code != 400 || !strings.Contains(body, "CONFIRM_MISMATCH") {
		t.Fatalf("confirmation : %d %s", code, body)
	}
	if code, _ := do("POST", path+"/cards/"+high+"/approve", "a", `{"confirm":"s1"}`); code != 200 || len(p.executed) != 1 {
		t.Fatalf("validation confirmée : %d", code)
	}

	// L'état a empiré entre la proposition et la validation : pas joué.
	p.risk = action.RiskLow
	low := prop("traefik")
	p.risk = action.RiskMedium
	code, body := do("POST", path+"/cards/"+low+"/approve", "a", `{}`)
	if code != 200 || !strings.Contains(body, `"status":"failed"`) || !strings.Contains(body, "le risque est passé de low à medium") || len(p.executed) != 1 {
		t.Fatalf("revérification : %d %s", code, body)
	}

	// Échec de l'exécution : carte en échec, entrée de journal dédiée.
	p.fail = errors.New("node-agent : cooldown")
	failing := prop("valkey")
	if code, body := do("POST", path+"/cards/"+failing+"/approve", "a", `{}`); code != 200 || !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("échec : %d %s", code, body)
	}
	if !strings.Contains(journalKinds(m), "card.execution_error") {
		t.Fatal(journalKinds(m))
	}

	// Refus, avec sa raison dans le dossier.
	p.fail = nil
	refused := prop("api")
	if code, body := do("POST", path+"/cards/"+refused+"/reject", "a", `{"note":"on attend la fenêtre de maintenance"}`); code != 200 || !strings.Contains(body, `"status":"rejected"`) {
		t.Fatalf("refus : %d %s", code, body)
	}
	if !strings.Contains(m.steps[len(m.steps)-1].Digest, "Raison : on attend la fenêtre") {
		t.Fatalf("étape : %+v", m.steps[len(m.steps)-1])
	}

	// Expirée.
	old := prop("docker")
	cards.now = cards.now.Add(store.CardTTL + time.Second)
	if code, _ := do("POST", path+"/cards/"+old+"/approve", "a", `{"confirm":"s1"}`); code != 409 {
		t.Fatalf("expirée : %d", code)
	}
	if code, _ := do("POST", path+"/cards/"+uuid.NewString()+"/approve", "a", `{}`); code != 404 {
		t.Fatalf("inconnue : %d", code)
	}

	// Dossier clos : plus de décision.
	m.inv.Status = store.StatusConcluded
	cards.now = time.Now()
	if code, _ := do("POST", path+"/cards/"+old+"/reject", "a", `{}`); code != 409 {
		t.Fatalf("dossier clos : %d", code)
	}
}

func TestCardsNotOfferedWithoutRight(t *testing.T) {
	model := llmtest.New("claude", llmtest.Say("ok"))
	_, do, m, _ := cardServer(t, model, &fakeProvider{risk: action.RiskLow})
	m.inv.ActorUserID = support.UserID.String()
	if code, _ := do("POST", "/v1/investigations/"+m.inv.ID.String()+"/messages", "s", `{"message":"redémarre k3s"}`); code != 200 {
		t.Fatal(code)
	}
	for _, tl := range model.Requests()[0].Tools {
		if strings.HasPrefix(tl.Name, proposePrefix) {
			t.Fatalf("outil de proposition offert au support : %s", tl.Name)
		}
	}
}

func TestHistoryOutcomes(t *testing.T) {
	steps := []store.Step{{Kind: store.StepQuestion, Digest: "q1"}, {Kind: store.StepAnswer, Digest: "a1"},
		{Kind: store.StepAction, Digest: "Carte X exécutée."}, {Kind: store.StepQuestion, Digest: "q2"}, {Kind: store.StepAnswer, Digest: "a2"},
		{Kind: store.StepAction, Digest: "Carte Y refusée."}}
	msgs, pending := History(steps)
	if len(msgs) != 4 || !strings.Contains(msgs[2].Text, "Carte X exécutée.") || !strings.HasSuffix(msgs[2].Text, "q2") || pending != "Carte Y refusée." {
		t.Fatalf("%+v %q", msgs, pending)
	}
}

func mustRegistry(t *testing.T, p action.Provider) *action.Registry {
	t.Helper()
	r, err := action.NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func httptestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// Une carte validée restée sans issue (arrêt pendant l'action) passe en
// échec, dans le journal et le dossier ; une action en cours n'est pas touchée.
func TestReapCards(t *testing.T) {
	_, m := setup(llmtest.New("claude"))
	now := time.Now()
	old, recent := now.Add(-ExecTimeout-2*time.Minute), now.Add(-time.Minute)
	inv := uuid.New()
	cards := &memCards{now: now, cards: []store.Card{
		{ID: uuid.New(), InvestigationID: inv, Kind: "test.service.restart", Target: "a", Status: store.CardApproved, DecidedBy: "awa@betorun.test", DecidedAt: &old},
		{ID: uuid.New(), InvestigationID: inv, Kind: "test.service.restart", Target: "b", Status: store.CardApproved, DecidedBy: "awa@betorun.test", DecidedAt: &recent},
	}}
	s := &Server{Store: m, Cards: cards, Logger: quietLogger()}
	s.ReapCards(context.Background())
	if cards.cards[0].Status != store.CardFailed || cards.cards[0].Result != interruptedNote || cards.cards[1].Status != store.CardApproved {
		t.Fatalf("cartes : %+v", cards.cards)
	}
	if k := journalKinds(m); !strings.Contains(k, "card.interrupted") {
		t.Fatalf("journal : %s", k)
	}
}

// L'arrêt attend une action en cours, au plus jusqu'au délai.
func TestDrain(t *testing.T) {
	s := &Server{}
	if !s.Drain(context.Background()) {
		t.Fatal("rien en cours")
	}
	s.running.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if s.Drain(ctx) {
		t.Fatal("action en cours ignorée")
	}
	go func() { time.Sleep(10 * time.Millisecond); s.running.Done() }()
	if !s.Drain(context.Background()) {
		t.Fatal("fin non vue")
	}
}
