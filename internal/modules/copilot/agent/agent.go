// Package agent : boucle du Copilot.
//
//	message → modèle (outils proposés) → appels d'outils → résultats → modèle → … → réponse
//
// Appel d'outils natif (internal/infrastructure/llm) : aucun JSON à extraire
// du texte, aucune relance « réponds au bon format ». Les freins (tours,
// appels d'outils, durée) viennent du Copilot SYK, qui les a éprouvés.
//
// Les résultats d'outils sont des DONNÉES : ils sont encadrés comme tels et
// le prompt système interdit d'y lire des consignes (journaux, README, noms
// de conteneurs peuvent contenir du texte piégé). Le Copilot ne lit que :
// le jeton délégué est en portée « read », une consigne piégée n'a donc
// aucun moyen d'écrire.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Limits : freins d'un tour.
type Limits struct {
	MaxIterations   int           // appels au modèle avec outils (défaut 6)
	MaxToolCalls    int           // appels d'outils au total (défaut 12)
	Timeout         time.Duration // durée totale du tour (défaut 120 s)
	MaxOutputTokens int           // par appel au modèle (défaut 1500)
}

func (l Limits) withDefaults() Limits {
	if l.MaxIterations <= 0 {
		l.MaxIterations = 6
	}
	if l.MaxToolCalls <= 0 {
		l.MaxToolCalls = 12
	}
	if l.Timeout <= 0 {
		l.Timeout = 120 * time.Second
	}
	if l.MaxOutputTokens <= 0 {
		l.MaxOutputTokens = 1500
	}
	return l
}

// ToolRunner : exécute un outil (tools.Catalog lié à la personne).
type ToolRunner interface {
	Definitions() []llm.Tool
	Run(ctx context.Context, name string, args json.RawMessage) tools.Result
}

// ScopedRunner : catalogue présenté par morceaux (noyau, puis outils nommés
// par find_features ou appelés par le modèle). Sans lui : tout le catalogue.
type ScopedRunner interface {
	Core() []string
	DefinitionsOf(names []string) []llm.Tool
}

// toolSet : outils présentés au modèle pendant un tour.
type toolSet struct {
	scoped ScopedRunner
	names  []string
	has    map[string]bool
}

func newToolSet(runner ToolRunner) *toolSet {
	s, ok := runner.(ScopedRunner)
	if !ok {
		return nil
	}
	ts := &toolSet{scoped: s, has: map[string]bool{}}
	ts.add(s.Core()...)
	return ts
}

func (ts *toolSet) add(names ...string) bool {
	added := false
	for _, n := range names {
		if !ts.has[n] {
			ts.has[n] = true
			ts.names = append(ts.names, n)
			added = true
		}
	}
	return added
}

func (ts *toolSet) defs() []llm.Tool { return ts.scoped.DefinitionsOf(ts.names) }

// Agent : sans état ; un Run par message.
type Agent struct {
	LLM    llm.Provider
	Limits Limits
	Now    func() time.Time
}

// Turn : un message de la personne et son contexte.
type Turn struct {
	System  string        // prompt système complet (voir Prompt)
	History []llm.Message // échanges précédents (texte seulement)
	Message string
	Caller  llm.Caller
}

// StopReason : pourquoi le tour s'est arrêté.
type StopReason string

const (
	StopComplete      StopReason = "complete"
	StopMaxIterations StopReason = "max_iterations"
	StopToolBudget    StopReason = "tool_budget"
	StopTimeout       StopReason = "timeout"
	StopCanceled      StopReason = "canceled"
	StopLLMError      StopReason = "llm_error"
)

// Result : fin d'un tour.
type Result struct {
	Answer    string
	Stop      StopReason
	ToolCalls int
	Usage     llm.Usage
}

// Emitter : événements du tour, dans l'ordre (texte, outils, fin).
type Emitter interface {
	Text(delta string)
	// ToolStart / ToolEnd : activité visible (« Je lis l'état du conteneur… »).
	ToolStart(callID, name string)
	ToolEnd(callID, name string, ok bool)
}

// ErrNoAnswer : le modèle n'a rien pu répondre (erreur dès le premier appel).
var ErrNoAnswer = errors.New("copilot: no answer")

// Run : un tour complet. Rend une erreur seulement si AUCUNE réponse n'a pu
// être produite ; sinon, le Result dit pourquoi le tour s'est arrêté.
func (a *Agent) Run(ctx context.Context, turn Turn, runner ToolRunner, emit Emitter) (Result, error) {
	lim := a.Limits.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	msgs := append(append([]llm.Message(nil), turn.History...), llm.Message{Role: llm.RoleUser, Text: turn.Message})
	set := newToolSet(runner)
	var defs []llm.Tool
	if set != nil {
		defs = set.defs()
	} else {
		defs = runner.Definitions()
	}
	var res Result
	var answer strings.Builder

	for iter := 0; ; iter++ {
		choice := llm.ToolChoice{}
		final := iter >= lim.MaxIterations || res.ToolCalls >= lim.MaxToolCalls
		if final {
			// Plus d'outils : le modèle répond avec ce qu'il a.
			choice = llm.ToolChoice{Mode: llm.ToolChoiceNone}
		}
		req := llm.Request{
			System: turn.System, Messages: msgs, Tools: defs, ToolChoice: choice,
			MaxOutputTokens: lim.MaxOutputTokens, Caller: turn.Caller,
		}
		resp, err := a.stream(ctx, req, emit, &answer)
		if err != nil {
			res.Answer = strings.TrimSpace(answer.String())
			res.Stop = stopFor(ctx, err)
			if res.Answer == "" && iter == 0 {
				return res, errors.Join(ErrNoAnswer, err)
			}
			return res, nil // réponse partielle : on rend ce qu'on a
		}
		addUsage(&res.Usage, resp.Usage)

		calls := resp.Message.ToolCalls
		if len(calls) == 0 || final {
			res.Answer = strings.TrimSpace(answer.String())
			res.Stop = StopComplete
			if final && iter >= lim.MaxIterations {
				res.Stop = StopMaxIterations
			} else if final {
				res.Stop = StopToolBudget
			}
			return res, nil
		}

		// Budget d'outils : on tronque le lot plutôt que de tout refuser.
		if left := lim.MaxToolCalls - res.ToolCalls; len(calls) > left {
			calls = calls[:left]
		}
		assistant := resp.Message
		assistant.ToolCalls = calls
		msgs = append(msgs, assistant)
		for _, call := range calls {
			emit.ToolStart(call.ID, call.Name)
			r := runner.Run(ctx, call.Name, call.Arguments)
			res.ToolCalls++
			emit.ToolEnd(call.ID, call.Name, !r.IsError)
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolResult: &llm.ToolResult{
				CallID: call.ID, Content: frame(call.Name, r), IsError: r.IsError,
			}})
			if set != nil {
				grown := false
				if !r.IsError {
					grown = set.add(call.Name) // outil existant appelé directement
				}
				if call.Name == tools.ToolFindFeatures && !r.IsError {
					grown = set.add(tools.NamedTools(r.Content)...) || grown
				}
				if grown {
					defs = set.defs()
				}
			}
		}
		// Le texte d'avant les outils (« Je regarde… ») reste affiché, mais
		// la réponse finale repart d'une ligne neuve.
		if answer.Len() > 0 && !strings.HasSuffix(answer.String(), "\n") {
			answer.WriteString("\n\n")
			emit.Text("\n\n")
		}
	}
}

// stream : un appel au modèle, texte transmis au fil de l'eau.
func (a *Agent) stream(ctx context.Context, req llm.Request, emit Emitter, answer *strings.Builder) (*llm.Response, error) {
	var resp *llm.Response
	for ev, err := range a.LLM.Stream(ctx, req) {
		if err != nil {
			return nil, err
		}
		switch ev.Kind {
		case llm.EventText:
			answer.WriteString(ev.Text)
			emit.Text(ev.Text)
		case llm.EventDone:
			resp = ev.Response
		}
	}
	if resp == nil {
		return nil, &llm.Error{Kind: llm.KindUnavailable, Message: "stream ended without a response"}
	}
	return resp, nil
}

// frame : le résultat d'un outil, présenté au modèle comme une donnée.
func frame(name string, r tools.Result) string {
	var b strings.Builder
	b.WriteString("<tool_result name=\"")
	b.WriteString(name)
	b.WriteString("\"")
	if r.Truncated {
		b.WriteString(" truncated=\"true\"")
	}
	b.WriteString(">\n")
	// Une fausse balise de fin dans le contenu ne doit pas « sortir » du cadre.
	b.WriteString(strings.ReplaceAll(r.Content, "</tool_result>", "</tool_result_>"))
	b.WriteString("\n</tool_result>")
	return b.String()
}

func stopFor(ctx context.Context, err error) StopReason {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StopTimeout
	case ctx.Err() != nil || llm.KindOf(err) == llm.KindCanceled:
		return StopCanceled
	}
	return StopLLMError
}

func addUsage(total *llm.Usage, u llm.Usage) {
	if !u.Known {
		return
	}
	total.Known = true
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
}
