package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

var statusTool = llm.Tool{Name: "get_status", Description: "statut d'un conteneur",
	Parameters: json.RawMessage(`{"type":"object","properties":{"workload":{"type":"string"}},"required":["workload"]}`)}

func baseReq() llm.Request {
	return llm.Request{
		System:          "Tu es le Copilot betorun.",
		Messages:        []llm.Message{{Role: llm.RoleUser, Text: "mon site est en panne ?"}},
		Tools:           []llm.Tool{statusTool},
		MaxOutputTokens: 256,
		Caller:          llm.Caller{OrgID: "org-1", UserID: "u-1"},
	}
}

func newClient(t *testing.T, h http.HandlerFunc, mut ...func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{Name: "test", BaseURL: srv.URL + "/v1", APIKey: "sk-secret", Model: "m-1",
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}, "model": "PIRATE"}}
	for _, m := range mut {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateSendsNativeToolsAndParsesToolCalls(t *testing.T) {
	var got map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-secret" {
			t.Errorf("chemin %s / auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"model":"m-1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
		  "tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_status","arguments":"{\"workload\":\"web\"}"}}]}}],
		  "usage":{"prompt_tokens":120,"completion_tokens":18}}`)
	})
	resp, err := c.Generate(context.Background(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != llm.StopToolUse || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_status" ||
		string(resp.Message.ToolCalls[0].Arguments) != `{"workload":"web"}` {
		t.Fatalf("réponse = %+v", resp)
	}
	if !resp.Usage.Known || resp.Usage.InputTokens != 120 || resp.Provider != "test" {
		t.Fatalf("usage/provider = %+v %s", resp.Usage, resp.Provider)
	}
	// Corps : modèle de la config (ExtraBody ne l'écrase pas), système en premier,
	// outils natifs, options du fournisseur, rien de Caller.
	if got["model"] != "m-1" || got["max_tokens"] != float64(256) || got["tool_choice"] != "auto" {
		t.Fatalf("corps = %v", got)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("système absent : %v", msgs[0])
	}
	if _, ok := got["chat_template_kwargs"]; !ok {
		t.Fatal("extra body absent")
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "org-1") || strings.Contains(string(raw), "u-1") {
		t.Fatal("Caller ne doit jamais partir chez le fournisseur")
	}
}

func TestGenerateRejectsUnknownOrNamelessTool(t *testing.T) {
	for _, name := range []string{"", "drop_database"} {
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant",
			  "tool_calls":[{"id":"x","type":"function","function":{"name":%q,"arguments":"{}"}}]}}]}`, name)
		})
		_, err := c.Generate(context.Background(), baseReq())
		if llm.KindOf(err) != llm.KindMalformedOutput {
			t.Fatalf("outil %q : %v", name, err)
		}
	}
}

func TestStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		header string
		want   llm.Kind
	}{
		{429, `{"error":{"message":"slow down"}}`, "7", llm.KindRateLimited},
		{401, `{"error":{"message":"bad key"}}`, "", llm.KindAuth},
		{400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, "", llm.KindContextTooLong},
		{400, `{"error":{"message":"bad param"}}`, "", llm.KindInvalidRequest},
		{503, `oops`, "", llm.KindUnavailable},
		{504, ``, "", llm.KindTimeout},
	}
	for _, tc := range cases {
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		})
		_, err := c.Generate(context.Background(), baseReq())
		e, ok := err.(*llm.Error)
		if !ok || e.Kind != tc.want || e.Status != tc.status {
			t.Fatalf("HTTP %d : %v", tc.status, err)
		}
		if tc.status == 429 && e.RetryAfter != 7*time.Second {
			t.Fatalf("Retry-After = %v", e.RetryAfter)
		}
		if strings.Contains(e.Error(), "sk-secret") {
			t.Fatal("la clé ne doit jamais apparaître dans une erreur")
		}
	}
}

func TestTimeout(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}, func(cfg *Config) { cfg.Timeout = 50 * time.Millisecond })
	_, err := c.Generate(context.Background(), baseReq())
	if llm.KindOf(err) != llm.KindTimeout {
		t.Fatalf("délai : %v", err)
	}
}

// Flux réaliste (format vLLM/OpenAI) : texte, appel d'outil en fragments,
// trame finale d'usage sans choix, commentaire de maintien, CRLF.
const sseBody = ": ping\r\n\r\n" +
	`data: {"model":"m-1","choices":[{"delta":{"role":"assistant","content":"Je "}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"content":"regarde."}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_status","arguments":"{\"work"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"load\":\"web\"}"}}]}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":9}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestStream(t *testing.T) {
	var body map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseBody)
	})
	var text string
	var calls []llm.ToolCall
	var done *llm.Response
	for ev, err := range c.Stream(context.Background(), baseReq()) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case llm.EventText:
			text += ev.Text
		case llm.EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case llm.EventDone:
			done = ev.Response
		}
	}
	if text != "Je regarde." || len(calls) != 1 || string(calls[0].Arguments) != `{"workload":"web"}` {
		t.Fatalf("texte=%q appels=%+v", text, calls)
	}
	if done == nil || done.Stop != llm.StopToolUse || done.Usage.InputTokens != 50 || !done.Usage.Known {
		t.Fatalf("done = %+v", done)
	}
	if body["stream"] != true || body["stream_options"] == nil {
		t.Fatalf("options de flux absentes : %v", body)
	}
}

func TestStreamCutBeforeFinish(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"Je "}}]}`+"\n\n")
	})
	var last error
	for _, err := range c.Stream(context.Background(), baseReq()) {
		last = err
	}
	if llm.KindOf(last) != llm.KindUnavailable {
		t.Fatalf("flux coupé : %v", last)
	}
}

func TestToolResultsAndToolChoiceOnTheWire(t *testing.T) {
	var got map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"C'est réparé."}}]}`)
	})
	req := baseReq()
	req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "get_status"}
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_status", Arguments: json.RawMessage(`{"workload":"web"}`)}}},
		llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{CallID: "c1", Content: "CrashLoopBackOff", IsError: true}},
	)
	resp, err := c.Generate(context.Background(), req)
	if err != nil || resp.Message.Text != "C'est réparé." || resp.Stop != llm.StopEndTurn {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	msgs := got["messages"].([]any)
	asst := msgs[2].(map[string]any)
	tool := msgs[3].(map[string]any)
	if asst["tool_calls"] == nil || tool["role"] != "tool" || tool["tool_call_id"] != "c1" ||
		!strings.HasPrefix(tool["content"].(string), "ERREUR") {
		t.Fatalf("messages = %v", msgs)
	}
	choice := got["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "get_status" {
		t.Fatalf("tool_choice = %v", got["tool_choice"])
	}
}
