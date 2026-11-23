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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/domain"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/grounding"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/store"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// ── Métriques (labels bornés : nom d'outil = catalogue fixe) ──────────────

// ── Contrôleur ─────────────────────────────────────────────────────────────

type Controller struct {
	Service *service.Service
}

func NewController(s *service.Service) *Controller { return &Controller{Service: s} }

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
	Message        string              `json:"message"`
	ConversationID string              `json:"conversation_id"`
	Page           PageContext         `json:"page"`
	Attachments    []domain.Attachment `json:"attachments"`
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
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, domain.MaxChatBody)).Decode(&in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(c, http.StatusRequestEntityTooLarge, domain.CodeAttachmentsTooLarge, "attached files must total 128 KB at most")
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
	if err := domain.CheckAttachments(in.Attachments); err != nil {
		var ae *domain.AttachmentError
		status := http.StatusBadRequest
		if errors.As(err, &ae) && ae.Code == domain.CodeAttachmentsTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		jsonError(c, status, ae.Code, ae.Message)
		return
	}
	convID := in.ConversationID
	if _, err := uuid.Parse(convID); err != nil {
		convID = uuid.NewString()
	}
	ctx := c.Request.Context()
	if retry, ok := s.Allow(ctx, user.String()); !ok {
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
	s.Mu.RLock()
	runner := &service.BoundRunner{Cat: s.Catalog, Inv: tools.Invoker{
		Handler: s.Handler, Token: token, ClientIP: c.ClientIP(),
		AcceptLanguage: c.GetHeader("Accept-Language"),
	}, Svc: s, UserID: user.String(), ConversationID: convID,
		OnProposal: func(v service.View) { sse.send("action", v) }}
	s.Mu.RUnlock()

	sse.send("meta", gin.H{"conversation_id": convID})

	// Preuves (lot A3) : jetons des tours précédents, message et pièces
	// jointes, page, échanges gardés ; les résultats d'outils s'y ajoutent.
	ev := grounding.NewEvidence()
	if ts, err := s.Store.LoadEvidence(ctx, user.String(), convID); err == nil {
		ev.AddTokens(ts)
	}
	for _, m := range stored {
		ev.Add(m.Text)
	}
	runner.Evidence = ev

	page := agent.Page{Path: in.Page.Path, ProjectID: in.Page.ProjectID, WorkloadID: in.Page.WorkloadID,
		BuildID: in.Page.BuildID, SiteID: in.Page.SiteID}
	turn := agent.Turn{
		System:  agent.Prompt(s.CurrentTime(), agent.Language(c.GetHeader("Accept-Language")), "", page),
		History: store.History(stored),
		Message: domain.WithAttachments(in.Message, in.Attachments),
		// Budget par personne et par jour (llm.Budget) : pas encore par organisation.
		Caller: llm.Caller{OrgID: "user:" + user.String(), UserID: user.String(), TurnID: uuid.NewString()},
	}
	ev.Add(turn.Message)
	ev.Add(strings.Join([]string{page.Path, page.ProjectID, page.WorkloadID, page.BuildID, page.SiteID}, " "))
	ag := &agent.Agent{LLM: s.LLM, Limits: lim}
	res, err := ag.Run(ctx, turn, runner, sse)
	service.TurnsTotal.WithLabelValues(string(res.Stop)).Inc()
	if err != nil {
		code, msg := errorCode(err)
		s.Logger.Warnw("copilot: no answer", "kind", llm.KindOf(err), "error", err)
		sse.send("error", gin.H{"code": code, "message": msg})
		return
	}
	if res.Answer != "" {
		res.Answer = h.verify(ctx, ag, turn, runner, sse, res.Answer, ev, c.GetHeader("Accept-Language"))
		if err := s.Store.SaveEvidence(context.WithoutCancel(ctx), user.String(), convID, ev.Tokens()); err != nil {
			s.Logger.Warnw("copilot: evidence not saved", "error", err)
		}
	}
	if res.Answer != "" {
		// Contexte détaché : la personne a pu fermer l'onglet, l'échange a eu lieu.
		if err := s.Store.Append(context.WithoutCancel(ctx), user.String(), convID, domain.HistoryNote(in.Message, in.Attachments), res.Answer); err != nil {
			s.Logger.Warnw("copilot: history not saved", "error", err)
		}
	}
	sse.send("done", gin.H{"answer": res.Answer, "stop": res.Stop, "tool_calls": res.ToolCalls})
}

// errorCode : erreur du modèle → code stable pour la console.
// verify : la réponse ne cite que des éléments prouvés. Sinon le modèle la
// réécrit une fois, avec la liste de ce qui n'a pas de preuve (il peut
// relire un outil) ; s'il insiste, ces éléments sont retirés. La console
// remplace la réponse affichée (événement replace).
func (h *Controller) verify(ctx context.Context, ag *agent.Agent, turn agent.Turn, runner *service.BoundRunner,
	sse *sse, answer string, ev *grounding.Evidence, acceptLanguage string) string {
	s := h.Service
	vs := grounding.Check(answer, ev, s.Grounding)
	if len(vs) == 0 {
		service.GroundingTotal.WithLabelValues("clean").Inc()
		return answer
	}
	for _, v := range vs {
		service.GroundingViolationsTotal.WithLabelValues(v.Kind).Inc()
	}
	s.Logger.Infow("copilot: answer not grounded, revising", "violations", vs)
	sse.send("revising", gin.H{})
	fix := turn
	fix.History = append(append([]llm.Message(nil), turn.History...),
		llm.Message{Role: llm.RoleUser, Text: turn.Message}, llm.Message{Role: llm.RoleAssistant, Text: answer})
	fix.Message = grounding.Feedback(vs)
	quiet := &toolEventsOnly{sse: sse}
	revised := answer
	if res, err := ag.Run(ctx, fix, runner, quiet); err == nil && strings.TrimSpace(res.Answer) != "" {
		revised = strings.TrimSpace(res.Answer)
	}
	outcome := "revised"
	if left := grounding.Check(revised, ev, s.Grounding); len(left) > 0 {
		label := "(non vérifié)"
		if agent.Language(acceptLanguage) == "English" {
			label = "(unverified)"
		}
		revised = grounding.Sanitize(revised, left, label)
		outcome = "sanitized"
		s.Logger.Warnw("copilot: answer sanitized", "violations", left)
	}
	service.GroundingTotal.WithLabelValues(outcome).Inc()
	sse.send("replace", gin.H{"answer": revised})
	return revised
}

// toolEventsOnly : pendant une réécriture, l'activité des outils reste
// visible ; le texte, lui, arrive d'un bloc (événement replace).
type toolEventsOnly struct{ sse *sse }

func (t *toolEventsOnly) Text(string)                      {}
func (t *toolEventsOnly) ToolStart(id, name string)        { t.sse.ToolStart(id, name) }
func (t *toolEventsOnly) ToolEnd(id, name string, ok bool) { t.sse.ToolEnd(id, name, ok) }

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
