package router

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/loopfz/gadgeto/tonic"
	"github.com/wI2L/fizz"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmsetup"
	"github.com/thekrauss/kubemanager/internal/modules/copilot"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/store"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// CopilotTool : une route de lecture proposée au Copilot comme outil
// (internal/modules/copilot/tools). Name en snake_case ; Fixed et Max
// encadrent les paramètres (ex. un flux de journaux lu sans le suivre).
type CopilotTool struct {
	Name  string
	Fixed map[string]string
	Max   map[string]int
	Hint  string
}

// ForCopilot : la route devient un outil du Copilot. Elle doit être une
// lecture (GET), publique et documentée : le schéma de l'outil vient de la
// spécification. Elle s'exécute avec les droits de la personne (jeton délégué).
func (r *Route) ForCopilot(t CopilotTool) *Route {
	r.Copilot = &t
	return r
}

// CopilotRoutes : routes marquées ForCopilot, avec leur chemin complet.
func CopilotRoutes(groups []*RouteGroup) []tools.Route {
	var out []tools.Route
	var walk func(g *RouteGroup, absPath string)
	walk = func(g *RouteGroup, absPath string) {
		for _, r := range g.Routes {
			if r.Copilot == nil {
				continue
			}
			out = append(out, tools.Route{
				Name: r.Copilot.Name, Method: r.Method, Path: buildFullPath(absPath, r.Path),
				Fixed: r.Copilot.Fixed, Max: r.Copilot.Max, Hint: r.Copilot.Hint,
			})
		}
		for _, child := range g.Groups {
			walk(child, absPath+child.Path)
		}
	}
	for _, g := range groups {
		walk(g, g.Path)
	}
	return out
}

// newCopilotService : le Copilot, désactivé (LLM nil) si llm.enabled=false
// ou si la pile llm ne se construit pas (erreur journalisée, l'API démarre).
func (a *App) newCopilotService() *copilot.Service {
	s := &copilot.Service{
		STS:    a.STS,
		Redis:  a.Redis,
		Logger: a.Logger,
		Config: copilot.Config{Limits: agent.Limits{Timeout: 120 * time.Second}},
	}
	if a.Redis != nil {
		s.Store = store.New(a.Redis)
	}
	if a.STS == nil || a.Redis == nil {
		a.Logger.Warnw("copilot disabled: STS or Redis unavailable")
		return s
	}
	provider, err := llmsetup.Build(a.Config.LLM, a.Redis)
	switch {
	case errors.Is(err, llmsetup.ErrDisabled):
		a.Logger.Infow("copilot disabled (llm.enabled=false)")
	case err != nil:
		a.Logger.Errorw("copilot disabled: llm configuration", "error", err)
	default:
		s.LLM = provider
		a.Logger.Infow("copilot enabled", "providers", provider.Name())
	}
	return s
}

// bindCopilot : une fois toutes les routes enregistrées, le Copilot reçoit le
// moteur HTTP (outils rejoués en interne) et ses outils, construits depuis la
// spécification publique en anglais.
func (a *App) bindCopilot(f *fizz.Fizz, h http.Handler) error {
	s := a.Services.Copilot
	if s == nil || s.LLM == nil {
		return nil
	}
	spec, err := publicSpec(f.Generator().API(), i18n.EN)
	if err != nil {
		return fmt.Errorf("copilot: spécification : %w", err)
	}
	return s.Bind(h, CopilotRoutes(AllGroups), spec)
}

// addCopilotRoutes : routes de la console vers le Copilot (internes : hors de
// la documentation publique, et jamais des outils elles-mêmes).
func addCopilotRoutes(app *App) {
	r := app.Controllers.Copilot
	if r == nil {
		return
	}
	g := RootGroup.NewGroup("/copilot", "Copilot").Internal()
	g.AddRoute("/status", http.MethodGet, "Le Copilot est-il disponible ?", Authenticated(), tonic.Handler(r.Status, http.StatusOK))
	g.AddRoute("/chat", http.MethodPost, "Message au Copilot, réponse en flux (SSE)", Authenticated(), r.Chat)
	g.AddRoute("/conversations/:id", http.MethodDelete, "Oublier une conversation", Authenticated(),
		tonic.Handler(r.ClearConversation, http.StatusNoContent))
}
