// Package openai : adaptateur llm.Provider pour toute API compatible
// « Chat Completions » d'OpenAI : OpenAI, vLLM (Qwen auto-hébergé), Groq,
// Gemini (point d'accès compatible), Mistral, OpenRouter, Ollama…
//
// Appel d'outils natif (champ tools / tool_calls). Aucun nouvel essai ici :
// la bascule et le disjoncteur sont des décorateurs du paquet llm.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// Config : un fournisseur compatible OpenAI.
type Config struct {
	// Name : nom stable, utilisé en label de métriques et dans les erreurs.
	Name    string
	BaseURL string // ex. https://api.openai.com/v1, http://vllm:8000/v1
	APIKey  string
	Model   string
	// Timeout : délai total d'un appel (flux compris). 120 s par défaut.
	Timeout time.Duration
	// MaxTokensField : "max_tokens" (défaut, vLLM/Groq/Gemini) ou
	// "max_completion_tokens" (modèles OpenAI récents).
	MaxTokensField string
	// ExtraBody : champs ajoutés tels quels au corps, ex. pour Qwen sur vLLM :
	// {"chat_template_kwargs": {"enable_thinking": false}}.
	ExtraBody map[string]any
	// HTTPClient : facultatif (tests).
	HTTPClient *http.Client
}

// maxBody : borne de lecture d'une réponse non-flux ou d'une erreur.
const maxBody = 8 << 20

type Client struct {
	cfg  Config
	http *http.Client
	url  string
}

func New(cfg Config) (*Client, error) {
	if cfg.Name == "" || cfg.BaseURL == "" || cfg.Model == "" {
		return nil, errors.New("openai: name, base_url et model sont obligatoires")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	if cfg.MaxTokensField != "max_tokens" && cfg.MaxTokensField != "max_completion_tokens" {
		return nil, fmt.Errorf("openai: max_tokens_field inconnu %q", cfg.MaxTokensField)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{} // délai porté par le contexte (flux longs)
	}
	return &Client{cfg: cfg, http: hc, url: strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"}, nil
}

func (c *Client) Name() string { return c.cfg.Name }

// ── Corps de requête ────────────────────────────────────────────────────────

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

func str(s string) *string { return &s }

func (c *Client) body(req llm.Request, stream bool) ([]byte, error) {
	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: str(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			msgs = append(msgs, wireMessage{Role: "user", Content: str(m.Text)})
		case llm.RoleAssistant:
			wm := wireMessage{Role: "assistant"}
			if m.Text != "" || len(m.ToolCalls) == 0 {
				wm.Content = str(m.Text)
			}
			for _, tc := range m.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID: tc.ID, Type: "function",
					Function: wireFunction{Name: tc.Name, Arguments: string(tc.Arguments)},
				})
			}
			msgs = append(msgs, wm)
		case llm.RoleTool:
			content := m.ToolResult.Content
			if m.ToolResult.IsError {
				// Pas de champ « erreur » dans ce format : on le dit en clair.
				content = "ERREUR de l'outil : " + content
			}
			msgs = append(msgs, wireMessage{Role: "tool", ToolCallID: m.ToolResult.CallID, Content: str(content)})
		}
	}

	b := map[string]any{}
	for k, v := range c.cfg.ExtraBody {
		b[k] = v
	}
	// Les champs du contrat passent APRÈS ExtraBody : la configuration ne peut
	// pas les écraser (modèle, messages, outils, plafond de jetons).
	b["model"] = c.cfg.Model
	b["messages"] = msgs
	b[c.cfg.MaxTokensField] = req.MaxOutputTokens
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters,
			}}
		}
		b["tools"] = tools
		switch req.ToolChoice.Mode {
		case llm.ToolChoiceNone:
			b["tool_choice"] = "none"
		case llm.ToolChoiceRequired:
			b["tool_choice"] = "required"
		case llm.ToolChoiceTool:
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice.Name}}
		default:
			b["tool_choice"] = "auto"
		}
	}
	if stream {
		b["stream"] = true
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(b)
}

// ── Réponse ─────────────────────────────────────────────────────────────────

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type wireChoice struct {
	Message      wireMessage `json:"message"`
	Delta        wireMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type wireResponse struct {
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
}

func stopReason(s string) llm.StopReason {
	switch s {
	case "stop":
		return llm.StopEndTurn
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	case "content_filter":
		return llm.StopContentFilter
	}
	return llm.StopUnknown
}

func usage(u *wireUsage) llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	return llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, Known: true}
}

// finish : vérifie les appels d'outils et complète la réponse. Un modèle qui
// appelle des outils sans le dire (finish_reason "stop") est corrigé : ce sont
// les appels qui font foi.
func (c *Client) finish(req llm.Request, resp *llm.Response) error {
	resp.Provider = c.cfg.Name
	if resp.Model == "" {
		resp.Model = c.cfg.Model
	}
	if err := llm.CheckToolCalls(c.cfg.Name, req, resp.Message.ToolCalls); err != nil {
		return err
	}
	if len(resp.Message.ToolCalls) > 0 && resp.Stop == llm.StopEndTurn {
		resp.Stop = llm.StopToolUse
	}
	if resp.Stop == llm.StopContentFilter {
		return &llm.Error{Kind: llm.KindContentFiltered, Provider: c.cfg.Name, Message: "réponse bloquée par le filtre du fournisseur"}
	}
	return nil
}

// ── Appels ──────────────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, req llm.Request, stream bool) (*http.Response, context.CancelFunc, error) {
	if err := req.Validate(); err != nil {
		var e *llm.Error
		if errors.As(err, &e) {
			e.Provider = c.cfg.Name
		}
		return nil, nil, err
	}
	payload, err := c.body(req, stream)
	if err != nil {
		return nil, nil, &llm.Error{Kind: llm.KindInvalidRequest, Provider: c.cfg.Name, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, nil, &llm.Error{Kind: llm.KindInvalidRequest, Provider: c.cfg.Name, Err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	if c.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		cancel()
		return nil, nil, c.transportError(ctx, err)
	}
	if hresp.StatusCode/100 != 2 {
		defer cancel()
		defer hresp.Body.Close()
		return nil, nil, c.statusError(hresp)
	}
	return hresp, cancel, nil
}

func (c *Client) transportError(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return &llm.Error{Kind: llm.KindTimeout, Provider: c.cfg.Name, Err: err}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &llm.Error{Kind: llm.KindTimeout, Provider: c.cfg.Name, Err: err}
	}
	return &llm.Error{Kind: llm.KindUnavailable, Provider: c.cfg.Name, Err: err}
}

func (c *Client) statusError(r *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	msg := providerMessage(raw)
	e := &llm.Error{Provider: c.cfg.Name, Status: r.StatusCode, Message: msg}
	switch {
	case r.StatusCode == http.StatusTooManyRequests:
		e.Kind = llm.KindRateLimited
		e.RetryAfter = retryAfter(r.Header.Get("Retry-After"))
	case r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden:
		e.Kind = llm.KindAuth
	case r.StatusCode == http.StatusRequestTimeout || r.StatusCode == http.StatusGatewayTimeout:
		e.Kind = llm.KindTimeout
	case r.StatusCode >= 500:
		e.Kind = llm.KindUnavailable
	case isContextLength(msg):
		e.Kind = llm.KindContextTooLong
	default:
		e.Kind = llm.KindInvalidRequest
	}
	return e
}

// providerMessage : message d'erreur du fournisseur, tronqué (jamais le corps
// entier, qui peut reprendre une partie du prompt).
func providerMessage(raw []byte) string {
	var w struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &w) == nil {
		msg = w.Error.Message
		if msg == "" {
			msg = w.Message
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

func isContextLength(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context_length") || strings.Contains(m, "context length") ||
		strings.Contains(m, "maximum context") || strings.Contains(m, "too many tokens")
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	hresp, cancel, err := c.do(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody))
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil || len(w.Choices) == 0 {
		return nil, &llm.Error{Kind: llm.KindMalformedOutput, Provider: c.cfg.Name, Message: "réponse illisible ou sans choix"}
	}
	ch := w.Choices[0]
	resp := &llm.Response{Model: w.Model, Usage: usage(w.Usage), Stop: llm.StopUnknown}
	if ch.FinishReason != nil {
		resp.Stop = stopReason(*ch.FinishReason)
	}
	resp.Message = llm.Message{Role: llm.RoleAssistant}
	if ch.Message.Content != nil {
		resp.Message.Text = *ch.Message.Content
	}
	for i, tc := range ch.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "call_" + strconv.Itoa(i) // certains serveurs vLLM l'omettent
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, llm.ToolCall{
			ID: id, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	if err := c.finish(req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		hresp, cancel, err := c.do(ctx, req, true)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		defer cancel()
		defer hresp.Body.Close()

		acc := newAccumulator()
		err = readSSE(hresp.Body, func(data []byte) (bool, error) {
			var w wireResponse
			if err := json.Unmarshal(data, &w); err != nil {
				return false, &llm.Error{Kind: llm.KindMalformedOutput, Provider: c.cfg.Name, Message: "trame de flux illisible"}
			}
			text := acc.add(w)
			if text != "" {
				if !yield(llm.Event{Kind: llm.EventText, Text: text}, nil) {
					return false, errStop
				}
			}
			return true, nil
		})
		if errors.Is(err, errStop) {
			return
		}
		if err != nil {
			var e *llm.Error
			if !errors.As(err, &e) {
				err = c.transportError(ctx, err)
			}
			yield(llm.Event{}, err)
			return
		}
		if !acc.finished {
			yield(llm.Event{}, &llm.Error{Kind: llm.KindUnavailable, Provider: c.cfg.Name, Message: "flux interrompu avant la fin"})
			return
		}
		resp := acc.response()
		if err := c.finish(req, resp); err != nil {
			yield(llm.Event{}, err)
			return
		}
		for i := range resp.Message.ToolCalls {
			if !yield(llm.Event{Kind: llm.EventToolCall, ToolCall: &resp.Message.ToolCalls[i]}, nil) {
				return
			}
		}
		yield(llm.Event{Kind: llm.EventDone, Response: resp}, nil)
	}
}

var errStop = errors.New("arrêt demandé par l'appelant")
