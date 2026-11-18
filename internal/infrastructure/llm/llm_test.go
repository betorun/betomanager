package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
)

var echoTool = llm.Tool{Name: "get_status", Description: "statut", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}

func req() llm.Request {
	return llm.Request{
		Messages:        []llm.Message{{Role: llm.RoleUser, Text: "bonjour"}},
		Tools:           []llm.Tool{echoTool},
		MaxOutputTokens: 100,
		Caller:          llm.Caller{OrgID: "org-1"},
	}
}

func kind(t *testing.T, err error, want llm.Kind) {
	t.Helper()
	if got := llm.KindOf(err); got != want {
		t.Fatalf("kind = %q, attendu %q (err=%v)", got, want, err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(r *llm.Request){
		"sans plafond":       func(r *llm.Request) { r.MaxOutputTokens = 0 },
		"sans message":       func(r *llm.Request) { r.Messages = nil },
		"nom vide":           func(r *llm.Request) { r.Tools = []llm.Tool{{Name: "", Parameters: echoTool.Parameters}} },
		"nom invalide":       func(r *llm.Request) { r.Tools = []llm.Tool{{Name: "a b", Parameters: echoTool.Parameters}} },
		"doublon":            func(r *llm.Request) { r.Tools = append(r.Tools, echoTool) },
		"schéma non objet":   func(r *llm.Request) { r.Tools = []llm.Tool{{Name: "x", Parameters: json.RawMessage(`[]`)}} },
		"choix outil absent": func(r *llm.Request) { r.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "nope"} },
		"résultat orphelin": func(r *llm.Request) {
			r.Messages = append(r.Messages, llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{CallID: "c1"}})
		},
	}
	for name, mut := range cases {
		r := req()
		mut(&r)
		kind(t, r.Validate(), llm.KindInvalidRequest)
		_ = name
	}

	ok := req()
	ok.Messages = append(ok.Messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_status", Arguments: json.RawMessage(`{}`)}}},
		llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{CallID: "c1", Content: "ok"}},
	)
	if err := ok.Validate(); err != nil {
		t.Fatalf("conversation valide refusée : %v", err)
	}
}

// Le piège du Copilot SYK : un appel d'outil sans nom ne doit jamais passer.
func TestCheckToolCalls(t *testing.T) {
	r := req()
	for _, bad := range []llm.ToolCall{
		{ID: "1", Name: ""},
		{ID: "1", Name: "delete_everything", Arguments: json.RawMessage(`{}`)},
		{ID: "1", Name: "get_status", Arguments: json.RawMessage(`"pas un objet"`)},
		{ID: "1", Name: "get_status", Arguments: json.RawMessage(`{cassé`)},
	} {
		kind(t, llm.CheckToolCalls("p", r, []llm.ToolCall{bad}), llm.KindMalformedOutput)
	}
	calls := []llm.ToolCall{{ID: "1", Name: "get_status"}}
	if err := llm.CheckToolCalls("p", r, calls); err != nil || string(calls[0].Arguments) != "{}" {
		t.Fatalf("arguments vides : err=%v args=%s", err, calls[0].Arguments)
	}
}

func TestChainFailsOverOnlyWhenAllowed(t *testing.T) {
	ctx := context.Background()
	a := llmtest.New("a", llmtest.Fail(llm.KindUnavailable))
	b := llmtest.New("b", llmtest.Say("salut"))
	c, _ := llm.NewChain(a, b)
	resp, err := c.Generate(ctx, req())
	if err != nil || resp.Provider != "b" {
		t.Fatalf("bascule attendue vers b : resp=%+v err=%v", resp, err)
	}

	// Requête invalide : aucun autre fournisseur ne fera mieux.
	a = llmtest.New("a", llmtest.Fail(llm.KindContextTooLong))
	b = llmtest.New("b", llmtest.Say("salut"))
	c, _ = llm.NewChain(a, b)
	_, err = c.Generate(ctx, req())
	kind(t, err, llm.KindContextTooLong)
	if len(b.Requests()) != 0 {
		t.Fatal("b n'aurait pas dû être appelé")
	}

	// Sortie malformée (outil sans nom) : panne du fournisseur, on bascule.
	a = llmtest.New("a", llmtest.Call("c1", "", `{}`))
	b = llmtest.New("b", llmtest.Say("salut"))
	c, _ = llm.NewChain(a, b)
	if resp, err = c.Generate(ctx, req()); err != nil || resp.Provider != "b" {
		t.Fatalf("bascule sur sortie malformée : %v", err)
	}
}

func TestChainStreamNeverSplicesAfterFirstEvent(t *testing.T) {
	ctx := context.Background()
	// a échoue avant tout événement : bascule.
	a := llmtest.New("a", llmtest.Fail(llm.KindRateLimited))
	b := llmtest.New("b", llmtest.Say("un deux trois"))
	c, _ := llm.NewChain(a, b)
	var text string
	var done *llm.Response
	for ev, err := range c.Stream(ctx, req()) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == llm.EventText {
			text += ev.Text
		}
		if ev.Kind == llm.EventDone {
			done = ev.Response
		}
	}
	if text != "un deux trois" || done == nil || done.Provider != "b" {
		t.Fatalf("texte=%q done=%+v", text, done)
	}

	// Arrêt anticipé par l'appelant : pas d'appel au suivant.
	b2 := llmtest.New("b", llmtest.Say("x"))
	c, _ = llm.NewChain(llmtest.New("a", llmtest.Say("un deux trois")), b2)
	for range c.Stream(ctx, req()) {
		break
	}
	if len(b2.Requests()) != 0 {
		t.Fatal("le suivant ne doit pas être appelé après un arrêt de l'appelant")
	}
}

func TestBreakerOpensAndProbes(t *testing.T) {
	ctx := context.Background()
	p := llmtest.New("a",
		llmtest.Fail(llm.KindUnavailable), llmtest.Fail(llm.KindUnavailable), // ouvre (seuil 2)
		llmtest.Fail(llm.KindUnavailable), // essai raté : rouvre
		llmtest.Say("revenu"),             // essai réussi : referme
	)
	now := time.Unix(0, 0)
	b := llm.WithBreaker(p, llm.BreakerConfig{Failures: 2, Cooldown: time.Minute})
	llm.SetBreakerClock(b, func() time.Time { return now })

	for range 2 {
		_, _ = b.Generate(ctx, req())
	}
	_, err := b.Generate(ctx, req())
	kind(t, err, llm.KindCircuitOpen)
	if len(p.Requests()) != 2 {
		t.Fatalf("ouvert : le fournisseur ne doit plus être appelé (%d appels)", len(p.Requests()))
	}

	now = now.Add(2 * time.Minute)
	_, err = b.Generate(ctx, req())
	kind(t, err, llm.KindUnavailable) // essai raté
	_, err = b.Generate(ctx, req())
	kind(t, err, llm.KindCircuitOpen) // rouvert

	now = now.Add(2 * time.Minute)
	if _, err = b.Generate(ctx, req()); err != nil {
		t.Fatalf("essai réussi attendu : %v", err)
	}
	if _, err = b.Generate(ctx, req()); llm.KindOf(err) == llm.KindCircuitOpen {
		t.Fatal("le disjoncteur aurait dû se refermer")
	}

	// Une requête invalide n'ouvre jamais le disjoncteur.
	p2 := llmtest.New("b", llmtest.Fail(llm.KindInvalidRequest), llmtest.Fail(llm.KindInvalidRequest), llmtest.Fail(llm.KindInvalidRequest), llmtest.Say("ok"))
	b2 := llm.WithBreaker(p2, llm.BreakerConfig{Failures: 2, Cooldown: time.Minute})
	for range 3 {
		_, _ = b2.Generate(ctx, req())
	}
	if _, err := b2.Generate(ctx, req()); err != nil {
		t.Fatalf("une erreur de requête ne doit pas ouvrir le disjoncteur : %v", err)
	}
}

func TestBudgetReservesThenRefunds(t *testing.T) {
	ctx := context.Background()
	ledger := llm.NewMemoryLedger(1_000_000)
	pricing := llm.Pricing{InputMicroUSDPerMTok: 1_000_000, OutputMicroUSDPerMTok: 4_000_000} // 1 $ et 4 $ le million
	step := llmtest.Say("ok")
	step.Usage = llm.Usage{InputTokens: 1000, OutputTokens: 500, Known: true}
	b := llm.WithBudget(llmtest.New("a", step), ledger, pricing)

	if _, err := b.Generate(ctx, req()); err != nil {
		t.Fatal(err)
	}
	if got, want := ledger.Spent("org-1"), pricing.Cost(1000, 500); got != want {
		t.Fatalf("dépense réglée = %d, attendu le coût réel %d", got, want)
	}

	// Plafond : la réservation (MaxOutputTokens énorme) est refusée AVANT l'appel.
	big := req()
	big.MaxOutputTokens = 10_000_000
	p := llmtest.New("a", llmtest.Say("jamais"))
	_, err := llm.WithBudget(p, ledger, pricing).Generate(ctx, big)
	kind(t, err, llm.KindBudgetExceeded)
	if len(p.Requests()) != 0 {
		t.Fatal("le fournisseur ne doit pas être appelé au-delà du plafond")
	}

	// Échec certain de n'avoir rien coûté : réservation rendue.
	before := ledger.Spent("org-1")
	_, err = llm.WithBudget(llmtest.New("a", llmtest.Fail(llm.KindUnavailable)), ledger, pricing).Generate(ctx, req())
	kind(t, err, llm.KindUnavailable)
	if ledger.Spent("org-1") != before {
		t.Fatal("la réservation d'un appel refusé doit être rendue")
	}

	// Organisation obligatoire.
	anon := req()
	anon.Caller.OrgID = ""
	_, err = b.Generate(ctx, anon)
	kind(t, err, llm.KindInvalidRequest)
}

func TestBudgetKeepsReservationWhenStreamCut(t *testing.T) {
	ctx := context.Background()
	ledger := llm.NewMemoryLedger(1_000_000)
	pricing := llm.Pricing{InputMicroUSDPerMTok: 1_000_000, OutputMicroUSDPerMTok: 1_000_000}
	b := llm.WithBudget(llmtest.New("a", llmtest.Say("un deux trois quatre")), ledger, pricing)
	for range b.Stream(ctx, req()) {
		break // l'appelant part au premier mot
	}
	if ledger.Spent("org-1") == 0 {
		t.Fatal("flux commencé puis coupé : la réservation doit rester comptée")
	}
}

func TestErrorKindsOfContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if llm.CanFailover(ctx, &llm.Error{Kind: llm.KindUnavailable}) {
		t.Fatal("pas de bascule pour un appelant parti")
	}
	kind(t, context.Canceled, llm.KindCanceled)
	kind(t, errors.New("x"), llm.KindUnavailable)
}
