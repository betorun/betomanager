// Package llmtest : fournisseur scripté pour tester ce qui consomme un
// llm.Provider (la boucle du Copilot), sans réseau ni modèle.
package llmtest

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// Step : une réponse scriptée. Err non nil : l'appel échoue avec cette erreur.
type Step struct {
	Text      string
	ToolCalls []llm.ToolCall
	Usage     llm.Usage
	Err       error
}

// Say : réponse texte.
func Say(text string) Step {
	return Step{Text: text, Usage: llm.Usage{InputTokens: 10, OutputTokens: len(text)/4 + 1, Known: true}}
}

// Call : appel d'outil (arguments en JSON).
func Call(id, name, argsJSON string) Step {
	return Step{ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(argsJSON)}},
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5, Known: true}}
}

// Fail : échec de type kind.
func Fail(kind llm.Kind) Step {
	return Step{Err: &llm.Error{Kind: kind, Provider: "scripted"}}
}

// Provider : rend les Step dans l'ordre et garde les requêtes reçues. Les
// appels d'outils passent par llm.CheckToolCalls, comme un vrai adaptateur.
type Provider struct {
	NameValue string

	mu       sync.Mutex
	steps    []Step
	requests []llm.Request
}

func New(name string, steps ...Step) *Provider {
	return &Provider{NameValue: name, steps: steps}
}

func (p *Provider) Name() string { return p.NameValue }

// Requests : copie des requêtes reçues.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

func (p *Provider) next(req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if len(p.steps) == 0 {
		return nil, &llm.Error{Kind: llm.KindUnavailable, Provider: p.NameValue, Message: "script épuisé"}
	}
	s := p.steps[0]
	p.steps = p.steps[1:]
	if s.Err != nil {
		return nil, s.Err
	}
	calls := append([]llm.ToolCall(nil), s.ToolCalls...)
	resp := &llm.Response{
		Message:  llm.Message{Role: llm.RoleAssistant, Text: s.Text, ToolCalls: calls},
		Stop:     llm.StopEndTurn,
		Usage:    s.Usage,
		Provider: p.NameValue,
		Model:    "scripted",
	}
	if len(calls) > 0 {
		resp.Stop = llm.StopToolUse
	}
	if err := llm.CheckToolCalls(p.NameValue, req, resp.Message.ToolCalls); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *Provider) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	return p.next(req)
}

// Stream : découpe le texte en mots, puis les appels d'outils, puis EventDone.
func (p *Provider) Stream(_ context.Context, req llm.Request) iter.Seq2[llm.Event, error] {
	return func(yield func(llm.Event, error) bool) {
		resp, err := p.next(req)
		if err != nil {
			yield(llm.Event{}, err)
			return
		}
		for _, w := range strings.SplitAfter(resp.Message.Text, " ") {
			if w != "" && !yield(llm.Event{Kind: llm.EventText, Text: w}, nil) {
				return
			}
		}
		for i := range resp.Message.ToolCalls {
			if !yield(llm.Event{Kind: llm.EventToolCall, ToolCall: &resp.Message.ToolCalls[i]}, nil) {
				return
			}
		}
		yield(llm.Event{Kind: llm.EventDone, Response: resp}, nil)
	}
}
