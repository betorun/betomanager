// Package anthropic : adaptateur llm.Provider pour l'API Messages
// d'Anthropic (Claude), par le SDK officiel pour Go (ADR 0008, ADR 0016).
//
// Points propres à Claude :
//   - réflexion adaptative toujours active sur les modèles récents ; la
//     profondeur se règle par l'effort (Config.Effort), jamais par un budget ;
//   - l'historique d'un tour doit revenir à l'identique (blocs de réflexion
//     compris) : chaque réponse garde son contenu brut (llm.Native), renvoyé
//     tel quel au tour suivant. Un bloc devenu invalide (outils ajoutés en
//     cours de tour) est écarté par l'API (drop_block) au lieu d'un refus ;
//   - prompt système et outils en cache (même préfixe d'un appel à l'autre) ;
//   - un refus de sécurité est repris par le modèle de secours désigné par
//     l'API (fallbacks "default"), sinon rendu en KindContentFiltered ;
//   - pas de température (refusée par les modèles récents), pas d'appel
//     d'outil imposé (ToolChoiceRequired / ToolChoiceTool refusés).
//
// Aucun nouvel essai ici : la bascule et le disjoncteur sont des décorateurs
// du paquet llm.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// Config : un compte Anthropic et un modèle.
type Config struct {
	// Name : nom stable (métriques, erreurs, llm.Native.Provider).
	Name   string
	APIKey string
	Model  string // ex. claude-opus-5-5
	// Effort : low, medium, high, xhigh ou max ; vide : celui du modèle.
	Effort string
	// BaseURL : facultatif (tests, passerelle).
	BaseURL string
	// Timeout : délai total d'un appel (flux compris). 10 min par défaut.
	Timeout time.Duration
	// NoFallbacks : pas de reprise d'un refus par le modèle de secours de l'API.
	NoFallbacks bool
	// HTTPClient : facultatif (tests).
	HTTPClient *http.Client
}

var efforts = map[string]sdk.BetaOutputConfigEffort{
	"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
}

type Client struct {
	cfg Config
	api sdk.Client
}

func New(cfg Config) (*Client, error) {
	if cfg.Name == "" || cfg.Model == "" || cfg.APIKey == "" {
		return nil, errors.New("anthropic: name, api_key et model sont obligatoires")
	}
	if cfg.Effort != "" {
		if _, ok := efforts[cfg.Effort]; !ok {
			return nil, errors.New("anthropic: effort inconnu " + strconv.Quote(cfg.Effort))
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey), option.WithMaxRetries(0)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Client{cfg: cfg, api: sdk.NewClient(opts...)}, nil
}

func (c *Client) Name() string { return c.cfg.Name }

//  Requête ─

func (c *Client) params(req llm.Request) (sdk.BetaMessageNewParams, error) {
	p := sdk.BetaMessageNewParams{
		Model:     c.cfg.Model,
		MaxTokens: int64(req.MaxOutputTokens),
		Betas:     []sdk.AnthropicBeta{sdk.AnthropicBetaThinkingBindingControls2026_08_01},
		Thinking: sdk.BetaThinkingConfigParamUnion{OfAdaptive: &sdk.BetaThinkingConfigAdaptiveParam{
			BlockBinding: sdk.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: sdk.BetaThinkingPrefixMismatchBehaviorDropBlock},
		}},
		// Cache du préfixe (outils, système, messages déjà envoyés).
		CacheControl: sdk.NewBetaCacheControlEphemeralParam(),
	}
	if e, ok := efforts[c.cfg.Effort]; ok {
		p.OutputConfig = sdk.BetaOutputConfigParam{Effort: e}
	}
	if !c.cfg.NoFallbacks {
		p.Betas = append(p.Betas, sdk.AnthropicBetaServerSideFallback2026_07_01)
		p.Fallbacks = sdk.BetaFallbacksParamOfDefault()
	}
	if req.System != "" {
		p.System = []sdk.BetaTextBlockParam{{Text: req.System}}
	}
	for _, t := range req.Tools {
		schema, err := inputSchema(t.Parameters)
		if err != nil {
			return p, err
		}
		tool := sdk.BetaToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tool.Description = sdk.String(t.Description)
		}
		p.Tools = append(p.Tools, sdk.BetaToolUnionParam{OfTool: &tool})
	}
	switch req.ToolChoice.Mode {
	case llm.ToolChoiceAuto:
	case llm.ToolChoiceNone:
		p.ToolChoice = sdk.BetaToolChoiceUnionParam{OfNone: &sdk.BetaToolChoiceNoneParam{}}
	default:
		// Les modèles récents refusent un appel d'outil imposé.
		return p, &llm.Error{Kind: llm.KindInvalidRequest, Provider: c.cfg.Name,
			Message: "appel d'outil imposé non pris en charge par ce modèle"}
	}
	p.Messages = c.messages(req.Messages)
	return p, nil
}

// inputSchema : schéma JSON d'un outil (objet) vers la forme du SDK.
func inputSchema(raw json.RawMessage) (sdk.BetaToolInputSchemaParam, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return sdk.BetaToolInputSchemaParam{}, err
	}
	s := sdk.BetaToolInputSchemaParam{Properties: m["properties"]}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				s.Required = append(s.Required, name)
			}
		}
	}
	extra := map[string]any{}
	for k, v := range m {
		if k != "type" && k != "properties" && k != "required" {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		s.ExtraFields = extra
	}
	return s, nil
}

// messages : conversation au format Anthropic. Les résultats d'outils qui se
// suivent forment UN message utilisateur (appels parallèles). Un message
// assistant produit par ce fournisseur revient tel quel.
func (c *Client) messages(in []llm.Message) []sdk.BetaMessageParam {
	var out []sdk.BetaMessageParam
	var results []sdk.BetaContentBlockParamUnion
	flush := func() {
		if len(results) > 0 {
			out = append(out, sdk.NewBetaUserMessage(results...))
			results = nil
		}
	}
	for _, m := range in {
		switch m.Role {
		case llm.RoleTool:
			if m.ToolResult != nil {
				results = append(results, sdk.NewBetaToolResultBlock(m.ToolResult.CallID, m.ToolResult.Content, m.ToolResult.IsError))
			}
			continue
		case llm.RoleUser:
			flush()
			out = append(out, sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(m.Text)))
		case llm.RoleAssistant:
			flush()
			if n := m.Native; n != nil && n.Provider == c.cfg.Name {
				if native, ok := n.Data.(sdk.BetaMessageParam); ok {
					out = append(out, native)
					continue
				}
			}
			var blocks []sdk.BetaContentBlockParamUnion
			if m.Text != "" {
				blocks = append(blocks, sdk.NewBetaTextBlock(m.Text))
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, sdk.NewBetaToolUseBlock(tc.ID, json.RawMessage(tc.Arguments), tc.Name))
			}
			if len(blocks) > 0 {
				out = append(out, sdk.BetaMessageParam{OfAssistant: &sdk.BetaAssistantMessageParam{Content: blocks}})
			}
		}
	}
	flush()
	return out
}

//  Réponse ─

func (c *Client) response(req llm.Request, msg *sdk.BetaMessage) (*llm.Response, error) {
	resp := &llm.Response{Provider: c.cfg.Name, Model: string(msg.Model), Stop: stopReason(msg.StopReason)}
	if resp.Model == "" {
		resp.Model = c.cfg.Model
	}
	resp.Message = llm.Message{Role: llm.RoleAssistant, Native: &llm.Native{Provider: c.cfg.Name, Data: msg.ToParam()}}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			text.WriteString(b.Text)
		case sdk.BetaToolUseBlock:
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, llm.ToolCall{
				ID: b.ID, Name: b.Name, Arguments: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	resp.Message.Text = text.String()
	u := msg.Usage
	resp.Usage = llm.Usage{
		InputTokens:  int(u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens),
		OutputTokens: int(u.OutputTokens), Known: true,
	}
	if err := llm.CheckToolCalls(c.cfg.Name, req, resp.Message.ToolCalls); err != nil {
		return nil, err
	}
	if len(resp.Message.ToolCalls) > 0 && resp.Stop == llm.StopEndTurn {
		resp.Stop = llm.StopToolUse
	}
	if resp.Stop == llm.StopContentFilter {
		return nil, &llm.Error{Kind: llm.KindContentFiltered, Provider: c.cfg.Name, Message: "réponse refusée par le modèle"}
	}
	return resp, nil
}

func stopReason(r sdk.BetaStopReason) llm.StopReason {
	switch r {
	case sdk.BetaStopReasonEndTurn, sdk.BetaStopReasonStopSequence:
		return llm.StopEndTurn
	case sdk.BetaStopReasonToolUse:
		return llm.StopToolUse
	case sdk.BetaStopReasonMaxTokens, sdk.BetaStopReasonModelContextWindowExceeded:
		return llm.StopMaxTokens
	case sdk.BetaStopReasonRefusal:
		return llm.StopContentFilter
	}
	return llm.StopUnknown
}

//  Appels

func (c *Client) prepare(req llm.Request) (sdk.BetaMessageNewParams, error) {
	if err := req.Validate(); err != nil {
		var e *llm.Error
		if errors.As(err, &e) {
			e.Provider = c.cfg.Name
		}
		return sdk.BetaMessageNewParams{}, err
	}
	return c.params(req)
}

// Generate : même chemin que le flux (réponses longues sans délai HTTP).
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	for ev, err := range c.Stream(ctx, req) {
		if err != nil {
			return nil, err
		}
		if ev.Kind == llm.EventDone {
			return ev.Response, nil
		}
	}
	return nil, &llm.Error{Kind: llm.KindUnavailable, Provider: c.cfg.Name, Message: "flux interrompu avant la fin"}
}

func (c *Client) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		p, err := c.prepare(req)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
		stream := c.api.Beta.Messages.NewStreaming(ctx, p)
		defer stream.Close()
		msg := sdk.BetaMessage{}
		for stream.Next() {
			ev := stream.Current()
			if err := msg.Accumulate(ev); err != nil {
				yield(llm.Event{}, &llm.Error{Kind: llm.KindMalformedOutput, Provider: c.cfg.Name, Message: "trame de flux illisible", Err: err})
				return
			}
			if d, ok := ev.AsAny().(sdk.BetaRawContentBlockDeltaEvent); ok {
				if t, ok := d.Delta.AsAny().(sdk.BetaTextDelta); ok && t.Text != "" {
					if !yield(llm.Event{Kind: llm.EventText, Text: t.Text}, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(llm.Event{}, c.classify(ctx, err))
			return
		}
		if msg.StopReason == "" {
			yield(llm.Event{}, &llm.Error{Kind: llm.KindUnavailable, Provider: c.cfg.Name, Message: "flux interrompu avant la fin"})
			return
		}
		resp, err := c.response(req, &msg)
		if err != nil {
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

// classify : erreur du SDK ou du transport vers llm.Error (jamais le corps
// de la requête, jamais la clé).
func (c *Client) classify(ctx context.Context, err error) error {
	var api *sdk.Error
	if errors.As(err, &api) {
		e := &llm.Error{Provider: c.cfg.Name, Status: api.StatusCode, Message: apiMessage(api)}
		switch {
		case api.StatusCode == http.StatusTooManyRequests:
			e.Kind = llm.KindRateLimited
			if api.Response != nil {
				e.RetryAfter = retryAfter(api.Response.Header.Get("Retry-After"))
			}
		case api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden:
			e.Kind = llm.KindAuth
		case api.StatusCode == http.StatusRequestTimeout || api.StatusCode == http.StatusGatewayTimeout:
			e.Kind = llm.KindTimeout
		case api.StatusCode >= 500: // 529 : surchargé
			e.Kind = llm.KindUnavailable
		case strings.Contains(strings.ToLower(e.Message), "prompt is too long"):
			e.Kind = llm.KindContextTooLong
		default:
			e.Kind = llm.KindInvalidRequest
		}
		return e
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
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

// apiMessage : message d'erreur de l'API, tronqué.
func apiMessage(e *sdk.Error) string {
	var w struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal([]byte(e.RawJSON()), &w) == nil {
		msg = w.Error.Message
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

func retryAfter(h string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}
