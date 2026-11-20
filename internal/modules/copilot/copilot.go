// Package copilot : le Copilot de la console betorun (lot 1 : lecture).
//
// Une personne écrit ; le Copilot lit l'état réel de ses ressources avec des
// outils qui sont les routes mêmes de l'API, rejouées en interne avec un
// jeton délégué en lecture (sts.MethodDelegated, portée read), et répond en
// flux (SSE). Voir docs/adr/0008 (couche llm) et le plan du Copilot.
//
//	POST /copilot/chat                     message (+ fichiers joints) → flux d'événements
//	GET  /copilot/status                   le Copilot est-il disponible ?
//	DELETE /copilot/conversations/:id      oublier une conversation
package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/store"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

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
	Config    Config
	Logger    *zap.SugaredLogger
	Now       func() time.Time

	mu      sync.RWMutex
	handler http.Handler
	catalog *tools.Catalog
}

// Bind : branche le moteur HTTP de l'API et construit les outils depuis la
// spécification publique (routes marquées ForCopilot). Appelé une fois les
// routes enregistrées.
func (s *Service) Bind(h http.Handler, routes []tools.Route, publicSpec []byte) error {
	cat, err := tools.Build(routes, publicSpec)
	if err != nil {
		return err
	}
	if err := tools.AddDiagnose(cat); err != nil {
		return err
	}
	s.mu.Lock()
	s.handler, s.catalog = h, cat
	s.mu.Unlock()
	return nil
}

// Enabled : modèle configuré et outils construits.
func (s *Service) Enabled() bool {
	if s == nil || s.LLM == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catalog != nil && s.handler != nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ── Métriques (labels bornés : nom d'outil = catalogue fixe) ──────────────

var (
	turnsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "turns_total",
		Help: "Messages traités par le Copilot, par raison d'arrêt.",
	}, []string{"stop"})
	toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "copilot", Name: "tool_calls_total",
		Help: "Appels d'outils du Copilot, par outil et résultat (ok|error).",
	}, []string{"tool", "outcome"})
)

// ── Contrôleur ─────────────────────────────────────────────────────────────

type Controller struct {
	Service *Service
}

func NewController(s *Service) *Controller { return &Controller{Service: s} }

// StatusResponse : disponibilité du Copilot pour la console.
type StatusResponse struct {
	Enabled bool `json:"enabled"`
}

func (h *Controller) Status(c *gin.Context) (*StatusResponse, error) {
	return &StatusResponse{Enabled: h.Service.Enabled()}, nil
}

// PageContext : écran d'où la personne écrit.
type PageContext struct {
	Path       string `json:"path"`
	ProjectID  string `json:"project_id"`
	WorkloadID string `json:"workload_id"`
	BuildID    string `json:"build_id"`
	SiteID     string `json:"site_id"`
}

// ChatRequest : un message, et ses fichiers joints (voir attachments.go).
type ChatRequest struct {
	Message        string       `json:"message"`
	ConversationID string       `json:"conversation_id"`
	Page           PageContext  `json:"page"`
	Attachments    []Attachment `json:"attachments"`
}

const maxMessageRunes = 4000

// ConversationPath : DELETE /copilot/conversations/:id.
type ConversationPath struct {
	ID string `path:"id" binding:"required,uuid"`
}

func (h *Controller) ClearConversation(c *gin.Context, in *ConversationPath) error {
	user, ok := security.InternalUserID(c)
	if !ok {
		return errUnauthorized
	}
	return h.Service.Store.Clear(c.Request.Context(), user.String(), in.ID)
}

var errUnauthorized = errors.New("authentication required")

func jsonError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"code": code, "error": msg, "message": msg})
}

// Chat : POST /copilot/chat, réponse en flux SSE.
//
// Événements : meta {conversation_id} · delta {text} · tool {id, name,
// status: running|ok|failed} · done {answer, stop} · error {code, message}.
// Les refus d'avant le flux (désactivé, quota, requête invalide) sont des
// réponses JSON ordinaires.
func (h *Controller) Chat(c *gin.Context) {
	s := h.Service
	if !s.Enabled() {
		jsonError(c, http.StatusServiceUnavailable, "COPILOT_DISABLED", "the Copilot is not available")
		return
	}
	user, okUser := security.InternalUserID(c)
	sessionID, acr, okSession := security.PersonSession(c)
	if !okUser || !okSession {
		jsonError(c, http.StatusForbidden, "COPILOT_PERSON_ONLY", "the Copilot is only available to a signed-in person")
		return
	}
	var in ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxChatBody)).Decode(&in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(c, http.StatusRequestEntityTooLarge, codeAttachmentsTooLarge, "attached files must total 128 KB at most")
			return
		}
		jsonError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	in.Message = strings.TrimSpace(in.Message)
	if in.Message == "" && len(in.Attachments) > 0 {
		in.Message = "(attached files)"
	}
	if in.Message == "" || len([]rune(in.Message)) > maxMessageRunes {
		jsonError(c, http.StatusBadRequest, "INVALID_REQUEST", "the message must contain 1 to 4000 characters")
		return
	}
	if err := checkAttachments(in.Attachments); err != nil {
		var ae *attachmentError
		status := http.StatusBadRequest
		if errors.As(err, &ae) && ae.code == codeAttachmentsTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		jsonError(c, status, ae.code, ae.message)
		return
	}
	convID := in.ConversationID
	if _, err := uuid.Parse(convID); err != nil {
		convID = uuid.NewString()
	}
	ctx := c.Request.Context()
	if retry, ok := s.allow(ctx, user.String()); !ok {
		c.Header("Retry-After", strconv.Itoa(retry))
		jsonError(c, http.StatusTooManyRequests, "COPILOT_RATE_LIMITED", "too many messages, please wait a moment")
		return
	}

	lim := s.Config.Limits
	ttl := lim.Timeout + 30*time.Second
	if lim.Timeout <= 0 {
		ttl = 3 * time.Minute
	}
	token, _, err := s.STS.MintDelegated(sts.Delegation{
		UserID: user, SessionID: sessionID, ACR: acr,
		Actor: sts.ActorCopilot, Scope: sts.ScopeRead, TTL: ttl,
	})
	if err != nil {
		s.Logger.Errorw("copilot: delegated token", "error", err)
		jsonError(c, http.StatusServiceUnavailable, "COPILOT_UNAVAILABLE", "the Copilot is temporarily unavailable")
		return
	}
	stored, err := s.Store.Load(ctx, user.String(), convID)
	if err != nil {
		s.Logger.Warnw("copilot: history unavailable", "error", err)
		stored = nil // sans mémoire, on répond quand même
	}

	sse := newSSE(c)
	s.mu.RLock()
	runner := &boundRunner{cat: s.catalog, inv: tools.Invoker{
		Handler: s.handler, Token: token, ClientIP: c.ClientIP(),
		AcceptLanguage: c.GetHeader("Accept-Language"),
	}, svc: s, userID: user.String(), conversationID: convID,
		onProposal: func(v View) { sse.send("action", v) }}
	s.mu.RUnlock()

	sse.send("meta", gin.H{"conversation_id": convID})

	page := agent.Page{Path: in.Page.Path, ProjectID: in.Page.ProjectID, WorkloadID: in.Page.WorkloadID,
		BuildID: in.Page.BuildID, SiteID: in.Page.SiteID}
	turn := agent.Turn{
		System:  agent.Prompt(s.now(), agent.Language(c.GetHeader("Accept-Language")), "", page),
		History: store.History(stored),
		Message: withAttachments(in.Message, in.Attachments),
		// Budget par personne et par jour (llm.Budget) : pas encore par organisation.
		Caller: llm.Caller{OrgID: "user:" + user.String(), UserID: user.String(), TurnID: uuid.NewString()},
	}
	ag := &agent.Agent{LLM: s.LLM, Limits: lim}
	res, err := ag.Run(ctx, turn, runner, sse)
	turnsTotal.WithLabelValues(string(res.Stop)).Inc()
	if err != nil {
		code, msg := errorCode(err)
		s.Logger.Warnw("copilot: no answer", "kind", llm.KindOf(err), "error", err)
		sse.send("error", gin.H{"code": code, "message": msg})
		return
	}
	if res.Answer != "" {
		// Contexte détaché : la personne a pu fermer l'onglet, l'échange a eu lieu.
		if err := s.Store.Append(context.WithoutCancel(ctx), user.String(), convID, historyNote(in.Message, in.Attachments), res.Answer); err != nil {
			s.Logger.Warnw("copilot: history not saved", "error", err)
		}
	}
	sse.send("done", gin.H{"answer": res.Answer, "stop": res.Stop, "tool_calls": res.ToolCalls})
}

// errorCode : erreur du modèle → code stable pour la console.
func errorCode(err error) (string, string) {
	switch llm.KindOf(err) {
	case llm.KindBudgetExceeded:
		return "COPILOT_BUDGET", "today's Copilot allowance is used up, it renews tomorrow"
	case llm.KindContextTooLong:
		return "COPILOT_TOO_LONG", "this conversation is too long, start a new one"
	case llm.KindCanceled:
		return "COPILOT_CANCELED", "the request was canceled"
	}
	return "COPILOT_UNAVAILABLE", "the Copilot is temporarily unavailable"
}

// allow : quota de messages par personne et par minute (Redis, fenêtre fixe).
// Redis injoignable : on refuse (même règle que le rate limit global, ADR 0006).
func (s *Service) allow(ctx context.Context, userID string) (retryAfter int, ok bool) {
	limit := s.Config.MessagesPerMinute
	if limit <= 0 {
		limit = 12
	}
	now := s.now().UTC()
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
type boundRunner struct {
	cat *tools.Catalog
	inv tools.Invoker
	// Écritures : propositions (cartes), une par message au plus.
	svc            *Service
	userID         string
	conversationID string
	onProposal     func(View)
	proposed       int
}

func (r *boundRunner) Definitions() []llm.Tool { return r.cat.Definitions() }

func (r *boundRunner) Run(ctx context.Context, name string, args json.RawMessage) tools.Result {
	if t, ok := r.cat.Lookup(name); ok && t.Write {
		res := r.propose(ctx, t, args)
		outcome := "proposed"
		if res.IsError {
			outcome = "error"
		}
		toolCallsTotal.WithLabelValues(name, outcome).Inc()
		return res
	}
	res := r.cat.Call(ctx, r.inv, name, args)
	outcome := "ok"
	if res.IsError {
		outcome = "error"
	}
	if _, known := r.cat.Lookup(name); !known {
		name = "unknown" // label borné
	}
	toolCallsTotal.WithLabelValues(name, outcome).Inc()
	return res
}

// ── SSE ────────────────────────────────────────────────────────────────────

type sse struct {
	c *gin.Context
}

func newSSE(c *gin.Context) *sse {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // proxys : pas de mise en tampon
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()
	return &sse{c: c}
}

func (s *sse) send(event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(s.c.Writer, "event: %s\ndata: %s\n\n", event, raw)
	s.c.Writer.Flush()
}

// agent.Emitter
func (s *sse) Text(delta string) { s.send("delta", gin.H{"text": delta}) }
func (s *sse) ToolStart(id, name string) {
	s.send("tool", gin.H{"id": id, "name": name, "status": "running"})
}
func (s *sse) ToolEnd(id, name string, ok bool) {
	status := "ok"
	if !ok {
		status = "failed"
	}
	s.send("tool", gin.H{"id": id, "name": name, "status": status})
}
