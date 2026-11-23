package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/grounding"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/store"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// propose : fige la requête, vérifie la ressource visée (elle existe et la
// personne y a accès), garde la proposition et la montre en carte.
func (r *BoundRunner) propose(ctx context.Context, t *tools.Tool, args json.RawMessage) tools.Result {
	if r.proposed >= 1 {
		return toolError("only one action can be proposed per message; wait for the person to validate the first one")
	}
	prepared, err := r.Cat.Prepare(t.Name, args)
	if err != nil {
		return toolError(err.Error())
	}
	target, err := r.target(ctx, t, prepared)
	if err != nil {
		return toolError(err.Error())
	}
	if t.ConfirmName && strings.TrimSpace(target) == "" {
		return toolError("the name of the resource could not be read: this action cannot be proposed")
	}
	token, err := NewToken()
	if err != nil {
		return toolError("the action could not be prepared")
	}
	now := r.Svc.CurrentTime().UTC()
	p := &Proposal{Token: token, UserID: r.UserID, ConversationID: r.ConversationID, Request: *prepared,
		Risk: t.Risk, Target: target, CreatedAt: now, ExpiresAt: now.Add(ProposalTTL),
		Strong: t.Strong, ConfirmName: t.ConfirmName}
	if err := r.Svc.ProposalsStore().Save(ctx, p, ProposalTTL); err != nil {
		r.Svc.Logger.Warnw("copilot: proposal not saved", "error", err)
		return toolError("the action could not be prepared")
	}
	r.proposed++
	view := p.View(t.PathParams())
	if r.OnProposal != nil {
		r.OnProposal(view)
	}
	// La carte (cible relue avec les droits de la personne, détails) est
	// une preuve : le modèle peut la décrire.
	if r.Evidence != nil {
		if raw, err := json.Marshal(view); err == nil {
			r.Evidence.Add(string(raw))
		}
	}
	return tools.Result{Status: http.StatusAccepted, Content: awaiting}
}

// target : nom lisible de la ressource visée, lu avec les droits de la
// personne. Une ressource introuvable ou interdite : pas de carte.
func (r *BoundRunner) target(ctx context.Context, t *tools.Tool, p *tools.Prepared) (string, error) {
	var tool string
	args := map[string]any{}
	switch {
	case t.TargetTool != "":
		// Outil de lecture déclaré : ses paramètres de chemin, pris de la carte.
		rd, _ := r.Cat.Lookup(t.TargetTool)
		tool = t.TargetTool
		for name := range rd.PathParams() {
			args[name] = p.Args[name]
		}
	case strings.Contains(t.Path, "/workloads/:id"):
		tool, args["id"] = tools.ToolGetWorkload, p.Args["id"]
	case strings.Contains(t.Path, ":siteID"):
		tool, args["id"], args["siteID"] = "get_site", p.Args["id"], p.Args["siteID"]
	case strings.Contains(t.Path, ":databaseID"):
		tool, args["id"], args["databaseID"] = "get_database", p.Args["id"], p.Args["databaseID"]
	case strings.Contains(t.Path, ":buildID"):
		tool, args["id"], args["buildID"] = "get_build", p.Args["id"], p.Args["buildID"]
	default:
		// Création (build, conteneur) : la ressource n'existe pas encore ; son
		// nom suffit, la route vérifiera les droits sur le projet.
		if name := scalarString(p.Args["name"]); name != "" {
			return name, nil
		}
		return "", nil
	}
	raw, _ := json.Marshal(args)
	res := r.Cat.Call(ctx, r.Inv, tool, raw)
	if res.IsError {
		return "", fmt.Errorf("the target resource was not found or is not accessible (HTTP %d)", res.Status)
	}
	var v struct {
		Name        string `json:"name"`
		Hostname    string `json:"hostname"`
		DisplayName string `json:"display_name"`
		AccountID   string `json:"account_id"`
		Title       string `json:"title"`
	}
	_ = json.Unmarshal([]byte(res.Content), &v)
	for _, n := range []string{v.Name, v.DisplayName, v.Hostname, v.AccountID, v.Title} {
		if strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n), nil
		}
	}
	return "", nil
}

func (s *Service) ProposalsStore() ProposalStore {
	if s.Proposals != nil {
		return s.Proposals
	}
	return &proposals{rdb: s.Redis}
}

// note : constat dans la conversation (sans mémoire configurée : rien).
func (s *Service) Note(ctx context.Context, userID, conversationID, text string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.AppendNote(ctx, userID, conversationID, text); err != nil {
		s.Logger.Warnw("copilot: action note not saved", "error", err)
	}
}

// Minter : jetons délégués (*sts.Service).
type Minter interface {
	MintDelegated(d sts.Delegation) (string, time.Time, error)
}

// Config : réglages du Copilot.
type Config struct {
	Limits agent.Limits
	// MessagesPerMinute : messages par personne et par minute (défaut 12).
	MessagesPerMinute int
}

// Service : le Copilot. LLM nil = désactivé (llm.enabled=false).
type Service struct {
	LLM   llm.Provider
	STS   Minter
	Store *store.Store
	Redis redis.UniversalClient
	// Proposals : cartes d'action (nil : Redis).
	Proposals ProposalStore
	// Plans : suivi des plans « mettre en ligne » (nil : Redis) ;
	// PlanTimings : leurs bornes (zéro : valeurs par défaut).
	Plans       PlanStore
	PlanTimings PlanTimings
	Config      Config
	Logger      *zap.SugaredLogger
	Now         func() time.Time

	Mu      sync.RWMutex
	Handler http.Handler
	Catalog *tools.Catalog
	// Grounding : ce qu'une réponse peut citer sans preuve (écrans, pages de
	// ressources, noms d'outils à ne jamais montrer), posé par Bind.
	Grounding grounding.Rules
}

// ResourcePages : pages de la console d'une ressource (identifiant à prouver).
var ResourcePages = []string{"/workloads/", "/builds/", "/sites/", "/vps/", "/support/"}

// Bind : branche le moteur HTTP de l'API et construit les outils depuis la
// spécification publique (routes marquées ForCopilot). Appelé une fois les
// routes enregistrées.
// FeatureMap : ce que la carte des fonctionnalités ajoute aux outils (guides,
// descriptions françaises, écrans de la console) ; nil : pas de carte.
type FeatureMap struct {
	RoutesFR map[string]string
	Guides   []tools.FeatureRoute
	Screens  []tools.Screen
}

func (s *Service) Bind(h http.Handler, routes []tools.Route, publicSpec []byte, features *FeatureMap) error {
	cat, err := tools.Build(routes, publicSpec)
	if err != nil {
		return err
	}
	if err := tools.AddDiagnose(cat); err != nil {
		return err
	}
	// Plan « mettre en ligne » : seulement si ses outils-routes sont là.
	if _, err := tools.AddLaunchPlan(cat); err != nil {
		return err
	}
	rules := grounding.Rules{Screens: map[string]bool{}, ResourcePages: ResourcePages, ToolNames: map[string]bool{}, Hosts: map[string]bool{}}
	if features != nil {
		if err := tools.AddFeatureMap(cat, publicSpec, features.RoutesFR, features.Guides, features.Screens); err != nil {
			return err
		}
		for _, sc := range features.Screens {
			rules.Screens[sc.Path] = true
		}
	}
	for _, n := range cat.Names() {
		rules.ToolNames[n] = true
	}
	s.Mu.Lock()
	s.Handler, s.Catalog, s.Grounding = h, cat, rules
	s.Mu.Unlock()
	return nil
}

// Enabled : modèle configuré et outils construits.
func (s *Service) Enabled() bool {
	if s == nil || s.LLM == nil {
		return false
	}
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.Catalog != nil && s.Handler != nil
}

func (s *Service) CurrentTime() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// allow : quota de messages par personne et par minute (Redis, fenêtre fixe).
// Redis injoignable : on refuse (même règle que le rate limit global, ADR 0006).
func (s *Service) Allow(ctx context.Context, userID string) (retryAfter int, ok bool) {
	limit := s.Config.MessagesPerMinute
	if limit <= 0 {
		limit = 12
	}
	now := s.CurrentTime().UTC()
	key := "betorun:copilot:rl:" + userID + ":" + now.Format("200601021504")
	n, err := s.Redis.Incr(ctx, key).Result()
	if err != nil {
		return 60, false
	}
	if n == 1 {
		s.Redis.Expire(ctx, key, 2*time.Minute)
	}
	if n > int64(limit) {
		return 60 - now.Second(), false
	}
	return 0, true
}

// boundRunner : le catalogue lié à la personne de la requête.
type BoundRunner struct {
	Cat *tools.Catalog
	Inv tools.Invoker
	// Écritures : propositions (cartes), une par message au plus.
	Svc            *Service
	UserID         string
	ConversationID string
	OnProposal     func(View)
	proposed       int
	// Evidence : preuves du tour (jetons des résultats d'outils), pour la
	// vérification de la réponse (nil : pas de vérification).
	Evidence *grounding.Evidence
}

func (r *BoundRunner) Definitions() []llm.Tool { return r.Cat.Definitions() }

// Core, DefinitionsOf : catalogue présenté par morceaux (agent.ScopedRunner).
func (r *BoundRunner) Core() []string                          { return r.Cat.Core() }
func (r *BoundRunner) DefinitionsOf(names []string) []llm.Tool { return r.Cat.DefinitionsOf(names) }

func (r *BoundRunner) Run(ctx context.Context, name string, args json.RawMessage) tools.Result {
	if t, ok := r.Cat.Lookup(name); ok && t.Write {
		var res tools.Result
		if name == tools.ToolProposeLaunchPlan {
			res = r.proposePlan(ctx, t, args)
		} else {
			res = r.propose(ctx, t, args)
		}
		outcome := "proposed"
		if res.IsError {
			outcome = "error"
		}
		toolCallsTotal.WithLabelValues(name, outcome).Inc()
		return res
	}
	res := r.Cat.Call(ctx, r.Inv, name, args)
	if r.Evidence != nil && !res.IsError {
		r.Evidence.Add(res.Content)
	}
	outcome := "ok"
	if res.IsError {
		outcome = "error"
	}
	if _, known := r.Cat.Lookup(name); !known {
		name = "unknown" // label borné
	}
	toolCallsTotal.WithLabelValues(name, outcome).Inc()
	return res
}

// awaiting : ce que le modèle reçoit quand il propose une action.
const awaiting = `{"status":"awaiting_validation","note":"A card was shown to the person. NOTHING has been done yet: ` +
	`it only runs if they validate it. Briefly tell them what the card will do and why; never say it is done."}`

func toolError(msg string) tools.Result {
	raw, _ := json.Marshal(map[string]string{"message": msg})
	return tools.Result{Content: string(raw), IsError: true}
}

func scalarString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

var (
	TurnsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "turns_total",
		Help: "Messages traités par le Copilot, par raison d'arrêt.",
	}, []string{"stop"})
	// GroundingTotal : vérification des réponses (clean : rien d'inventé ;
	// revised : corrigée par le modèle ; sanitized : éléments retirés).
	GroundingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "grounding_total",
		Help: "Réponses du Copilot vérifiées, par issue (clean|revised|sanitized).",
	}, []string{"outcome"})
	// GroundingViolationsTotal : éléments sans preuve trouvés, par sorte.
	GroundingViolationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "grounding_violations_total",
		Help: "Éléments de réponse sans preuve (première vérification), par sorte.",
	}, []string{"kind"})
	toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "tool_calls_total",
		Help: "Appels d'outils du Copilot, par outil et résultat (ok|error).",
	}, []string{"tool", "outcome"})
)
