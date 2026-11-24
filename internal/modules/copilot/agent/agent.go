// Package agent : boucle du Copilot, sur la boucle commune (internal/agentkit).
//
// Ce paquet garde le prompt du copilot et adapte son catalogue (résultats
// d'outils plus riches, outil find_features) à la boucle commune. Le Copilot
// ne lit que : le jeton délégué est en portée « read », une consigne piégée
// dans un résultat d'outil n'a donc aucun moyen d'écrire.
package agent

import (
	"context"
	"encoding/json"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Types de la boucle commune, sous leurs noms habituels.
type (
	Limits     = agentkit.Limits
	Turn       = agentkit.Turn
	Result     = agentkit.Result
	Emitter    = agentkit.Emitter
	StopReason = agentkit.StopReason
)

const (
	StopComplete      = agentkit.StopComplete
	StopMaxIterations = agentkit.StopMaxIterations
	StopToolBudget    = agentkit.StopToolBudget
	StopTimeout       = agentkit.StopTimeout
	StopCanceled      = agentkit.StopCanceled
	StopLLMError      = agentkit.StopLLMError
)

// ErrNoAnswer : le modèle n'a rien pu répondre (erreur dès le premier appel).
var ErrNoAnswer = agentkit.ErrNoAnswer

// ToolRunner : exécute un outil (tools.Catalog lié à la personne).
type ToolRunner interface {
	Definitions() []llm.Tool
	Run(ctx context.Context, name string, args json.RawMessage) tools.Result
}

// ScopedRunner : catalogue présenté par morceaux (noyau, puis outils nommés
// par find_features ou appelés par le modèle). Sans lui : tout le catalogue.
type ScopedRunner = agentkit.ScopedRunner

// Agent : sans état ; un Run par message.
type Agent agentkit.Agent

// Run : un tour complet (voir agentkit.Agent.Run).
func (a *Agent) Run(ctx context.Context, turn Turn, runner ToolRunner, emit Emitter) (Result, error) {
	return (*agentkit.Agent)(a).Run(ctx, turn, adapt(runner), emit)
}

// adapt : le catalogue du copilot vu par la boucle commune ; garde la
// présentation par morceaux et la découverte par find_features.
func adapt(r ToolRunner) agentkit.ToolRunner {
	base := runner{r}
	if s, ok := r.(ScopedRunner); ok {
		return scoped{base, s}
	}
	return base
}

type runner struct{ r ToolRunner }

func (a runner) Definitions() []llm.Tool { return a.r.Definitions() }

func (a runner) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	res := a.r.Run(ctx, name, args)
	return agentkit.ToolResult{Content: res.Content, IsError: res.IsError, Truncated: res.Truncated}
}

// Discover : find_features nomme les outils qui deviennent disponibles.
func (a runner) Discover(name, content string) []string {
	if name != tools.ToolFindFeatures {
		return nil
	}
	return tools.NamedTools(content)
}

type scoped struct {
	runner
	s ScopedRunner
}

func (a scoped) Core() []string                          { return a.s.Core() }
func (a scoped) DefinitionsOf(names []string) []llm.Tool { return a.s.DefinitionsOf(names) }
