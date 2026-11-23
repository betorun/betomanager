package router

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/loopfz/gadgeto/tonic"
	"github.com/wI2L/fizz"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmsetup"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	copilotSvc "github.com/thekrauss/kubemanager/internal/modules/copilot/service"
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
	// Write : action (POST/PATCH) proposée sur une carte que la personne
	// valide ; jamais exécutée par le modèle. Risk obligatoire.
	Write bool
	Risk  tools.Risk
	// Omit : champs du corps cachés au modèle (ex. secrets).
	Omit []string
	// Only : seuls champs du corps exposés (prioritaire sur Omit).
	Only []string
	// Undo : outil inverse (mêmes paramètres de chemin).
	Undo string
	// Strong : carte sensible (suppression, action sensible) : jouée avec la
	// session de la personne, double authentification exigée par l'API.
	Strong bool
	// ConfirmName : nom de la ressource à retaper (lu par TargetTool).
	ConfirmName bool
	TargetTool  string
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
			info, _ := iam.Info(r.Access.permission)
			out = append(out, tools.Route{
				Name: r.Copilot.Name, Method: r.Method, Path: buildFullPath(absPath, r.Path),
				Fixed: r.Copilot.Fixed, Max: r.Copilot.Max, Hint: r.Copilot.Hint,
				Write: r.Copilot.Write, Risk: r.Copilot.Risk, Omit: r.Copilot.Omit, Only: r.Copilot.Only, Undo: r.Copilot.Undo,
				Sensitive: info.Sensitive, Strong: r.Copilot.Strong, ConfirmName: r.Copilot.ConfirmName, TargetTool: r.Copilot.TargetTool,
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
func (a *App) newCopilotService() *copilotSvc.Service {
	s := &copilotSvc.Service{
		STS:    a.STS,
		Redis:  a.Redis,
		Logger: a.Logger,
		Config: copilotSvc.Config{Limits: agent.Limits{Timeout: 120 * time.Second}},
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
	fr, guides := CopilotFeatureInput(AllGroups)
	routes, guides, screens := withoutHiddenFeatures(CopilotRoutes(AllGroups), guides, CopilotScreens(), a.hiddenCopilotPaths())
	return s.Bind(h, routes, spec, &copilotSvc.FeatureMap{RoutesFR: fr, Guides: guides, Screens: screens})
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
	// Cartes d'action : la personne valide (l'action est jouée une fois, avec
	// ses droits) ou refuse. Jamais un jeton délégué : Authenticated() le refuse.
	g.AddRoute("/actions/:token/approve", http.MethodPost, "Valider une action proposée par le Copilot", Authenticated(), r.Approve)
	g.AddRoute("/actions/:token/reject", http.MethodPost, "Refuser une action proposée par le Copilot", Authenticated(), r.Reject)
	// Plans « mettre en ligne » (lot B) : suivi d'un plan validé, et demande
	// d'arrêt ; seulement par la personne qui l'a validé (sinon 404).
	g.AddRoute("/plans/:token", http.MethodGet, "État d'un plan « mettre en ligne » validé", Authenticated(), r.GetPlan)
	g.AddRoute("/plans/:token/pause", http.MethodPost, "Arrêter un plan « mettre en ligne » après l'étape en cours", Authenticated(), r.PausePlan)
}
