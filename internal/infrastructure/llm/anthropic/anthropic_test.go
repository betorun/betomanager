package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// fakeAPI : rejoue un flux SSE de l'API Messages et garde les requêtes.
type fakeAPI struct {
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	status  int
	retry   string
	events  []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if f.status != 0 {
		if f.retry != "" {
			w.Header().Set("Retry-After", f.retry)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"x","message":"refusé par le test"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range f.events {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(ev), &head)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, ev)
	}
}

const (
	msgStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":3,"cache_read_input_tokens":5}}}`
	msgStop  = `{"type":"message_stop"}`
)

func toolTurn() []string {
	return []string{
		msgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-123"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Je regarde "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"les pods."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"list_pods","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"namespace\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"km-web\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
		msgStop,
	}
}

func client(t *testing.T, f *fakeAPI, effort string) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{Name: "claude", APIKey: "test-key", Model: "claude-opus-5-5", Effort: effort, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func request() llm.Request {
	return llm.Request{
		System:          "Tu es l'Opérateur.",
		Messages:        []llm.Message{{Role: llm.RoleUser, Text: "Le site km-web est en panne."}},
		Tools:           []llm.Tool{{Name: "list_pods", Description: "Pods d'un namespace", Parameters: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"}},"required":["namespace"],"additionalProperties":false}`)}},
		MaxOutputTokens: 16000,
	}
}

func TestStreamToolCallAndReplay(t *testing.T) {
	f := &fakeAPI{events: toolTurn()}
	c := client(t, f, "high")
	var text strings.Builder
	var resp *llm.Response
	var calls int
	for ev, err := range c.Stream(context.Background(), request()) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case llm.EventText:
			text.WriteString(ev.Text)
		case llm.EventToolCall:
			calls++
		case llm.EventDone:
			resp = ev.Response
		}
	}
	if text.String() != "Je regarde les pods." || calls != 1 || resp == nil {
		t.Fatalf("texte %q, appels %d, réponse %v", text.String(), calls, resp)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "list_pods" || string(tc.Arguments) != `{"namespace":"km-web"}` {
		t.Fatalf("appel = %+v (%s)", tc, tc.Arguments)
	}
	if resp.Stop != llm.StopToolUse || resp.Usage != (llm.Usage{InputTokens: 18, OutputTokens: 42, Known: true}) {
		t.Fatalf("arrêt %s, usage %+v", resp.Stop, resp.Usage)
	}
	if resp.Message.Native == nil || resp.Message.Native.Provider != "claude" {
		t.Fatal("contenu natif absent")
	}

	// Requête : réflexion adaptative sans budget, blocs invalides écartés,
	// secours de l'API, effort, cache, aucune température.
	b := f.bodies[0]
	th := b["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["block_binding"].(map[string]any)["prefix_mismatch_behavior"] != "drop_block" || th["budget_tokens"] != nil {
		t.Fatalf("thinking = %v", th)
	}
	if b["fallbacks"] != "default" || b["output_config"].(map[string]any)["effort"] != "high" || b["cache_control"] == nil {
		t.Fatalf("corps = %v", b)
	}
	if _, ok := b["temperature"]; ok {
		t.Fatal("température envoyée")
	}
	if b["tool_choice"] != nil {
		t.Fatal("choix d'outil imposé")
	}
	tool := b["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "list_pods" || schema["type"] != "object" || schema["additionalProperties"] != false ||
		fmt.Sprint(schema["required"]) != "[namespace]" {
		t.Fatalf("outil = %v", tool)
	}
	betas := f.headers[0].Get("anthropic-beta")
	if !strings.Contains(betas, "thinking-binding-controls-2026-08-01") || !strings.Contains(betas, "server-side-fallback-2026-07-01") {
		t.Fatalf("betas = %q", betas)
	}
	if f.headers[0].Get("Authorization") != "Bearer test-key" {
		t.Fatal("clé absente")
	}

	// Tour suivant : le message assistant revient à l'identique (réflexion
	// signée comprise), les résultats d'outils dans un seul message.
	f.events = []string{msgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Le conteneur manque de mémoire."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
		msgStop}
	next := request()
	next.Messages = append(next.Messages, resp.Message,
		llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{CallID: "toolu_1", Content: "OOMKilled"}})
	final, err := c.Generate(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if final.Message.Text != "Le conteneur manque de mémoire." || final.Stop != llm.StopEndTurn {
		t.Fatalf("réponse = %+v", final)
	}
	msgs := f.bodies[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)
	first := assistant[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] != "sig-123" {
		t.Fatalf("réflexion non renvoyée : %v", assistant)
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Fatalf("résultat = %v", result)
	}
}

// Un message assistant venu d'un autre fournisseur (bascule) est reconstruit
// depuis le texte et les appels.
func TestForeignAssistantMessage(t *testing.T) {
	f := &fakeAPI{events: toolTurn()}
	c := client(t, f, "")
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, Text: "Je regarde.", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "list_pods", Arguments: json.RawMessage(`{"namespace":"a"}`)}},
			Native: &llm.Native{Provider: "gemini", Data: "ignoré"}},
		llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{CallID: "c1", Content: "ok"}})
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.bodies[0]["output_config"]; ok {
		t.Fatal("effort envoyé sans réglage")
	}
	blocks := f.bodies[0]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["text"] != "Je regarde." || blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant = %v", blocks)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		status int
		retry  string
		kind   llm.Kind
	}{
		{429, "7", llm.KindRateLimited},
		{401, "", llm.KindAuth},
		{529, "", llm.KindUnavailable},
		{400, "", llm.KindInvalidRequest},
	}
	for _, tc := range cases {
		f := &fakeAPI{status: tc.status, retry: tc.retry}
		_, err := client(t, f, "").Generate(context.Background(), request())
		if llm.KindOf(err) != tc.kind {
			t.Fatalf("HTTP %d : %v", tc.status, err)
		}
		var e *llm.Error
		if tc.retry != "" && (!errors.As(err, &e) || e.RetryAfter.Seconds() != 7) {
			t.Fatalf("Retry-After perdu : %v", err)
		}
		if strings.Contains(err.Error(), "test-key") {
			t.Fatal("clé dans l'erreur")
		}
	}
}

func TestRefusalAndForcedTool(t *testing.T) {
	f := &fakeAPI{events: []string{msgStart,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":1}}`, msgStop}}
	c := client(t, f, "")
	if _, err := c.Generate(context.Background(), request()); llm.KindOf(err) != llm.KindContentFiltered {
		t.Fatalf("refus : %v", err)
	}
	req := request()
	req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceRequired}
	if _, err := c.Generate(context.Background(), req); llm.KindOf(err) != llm.KindInvalidRequest {
		t.Fatalf("appel imposé : %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Name: "claude", Model: "claude-opus-5-5"}); err == nil {
		t.Fatal("clé absente acceptée")
	}
	if _, err := New(Config{Name: "claude", APIKey: "k", Model: "claude-opus-5-5", Effort: "turbo"}); err == nil {
		t.Fatal("effort inconnu accepté")
	}
}
