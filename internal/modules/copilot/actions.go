package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

var actionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "kubemanager", Subsystem: "copilot", Name: "actions_total",
	Help: "Cartes d'action du Copilot, par outil et issue (approved|rejected|failed|conflict).",
}, []string{"tool", "outcome"})

// awaiting : ce que le modèle reçoit quand il propose une action.
const awaiting = `{"status":"awaiting_validation","note":"A card was shown to the person. NOTHING has been done yet: ` +
	`it only runs if they validate it. Briefly tell them what the card will do and why; never say it is done."}`

// propose : fige la requête, vérifie la ressource visée (elle existe et la
// personne y a accès), garde la proposition et la montre en carte.
func (r *boundRunner) propose(ctx context.Context, t *tools.Tool, args json.RawMessage) tools.Result {
	if r.proposed >= 1 {
		return toolError("only one action can be proposed per message; wait for the person to validate the first one")
	}
	prepared, err := r.cat.Prepare(t.Name, args)
	if err != nil {
		return toolError(err.Error())
	}
	target, err := r.target(ctx, t, prepared)
	if err != nil {
		return toolError(err.Error())
	}
	token, err := newToken()
	if err != nil {
		return toolError("the action could not be prepared")
	}
	now := r.svc.now().UTC()
	p := &Proposal{Token: token, UserID: r.userID, ConversationID: r.conversationID, Request: *prepared,
		Risk: t.Risk, Target: target, CreatedAt: now, ExpiresAt: now.Add(ProposalTTL)}
	if err := r.svc.proposals().save(ctx, p, ProposalTTL); err != nil {
		r.svc.Logger.Warnw("copilot: proposal not saved", "error", err)
		return toolError("the action could not be prepared")
	}
	r.proposed++
	if r.onProposal != nil {
		r.onProposal(p.view(t.PathParams()))
	}
	return tools.Result{Status: http.StatusAccepted, Content: awaiting}
}

// target : nom lisible de la ressource visée, lu avec les droits de la
// personne. Une ressource introuvable ou interdite : pas de carte.
func (r *boundRunner) target(ctx context.Context, t *tools.Tool, p *tools.Prepared) (string, error) {
	var tool string
	args := map[string]any{}
	switch {
	case strings.Contains(t.Path, "/workloads/:id"):
		tool, args["id"] = tools.ToolGetWorkload, p.Args["id"]
	case strings.Contains(t.Path, ":siteID"):
		tool, args["id"], args["siteID"] = "get_site", p.Args["id"], p.Args["siteID"]
	case strings.Contains(t.Path, ":buildID"):
		tool, args["id"], args["buildID"] = "get_build", p.Args["id"], p.Args["buildID"]
	default:
		return "", nil
	}
	raw, _ := json.Marshal(args)
	res := r.cat.Call(ctx, r.inv, tool, raw)
	if res.IsError {
		return "", fmt.Errorf("the target resource was not found or is not accessible (HTTP %d)", res.Status)
	}
	var v struct {
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
	}
	_ = json.Unmarshal([]byte(res.Content), &v)
	if v.Name != "" {
		return v.Name, nil
	}
	return v.Hostname, nil
}

func toolError(msg string) tools.Result {
	raw, _ := json.Marshal(map[string]string{"message": msg})
	return tools.Result{Content: string(raw), IsError: true}
}

func (s *Service) proposals() ProposalStore {
	if s.Proposals != nil {
		return s.Proposals
	}
	return &proposals{rdb: s.Redis}
}

// note : constat dans la conversation (sans mémoire configurée : rien).
func (s *Service) note(ctx context.Context, userID, conversationID, text string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.AppendNote(ctx, userID, conversationID, text); err != nil {
		s.Logger.Warnw("copilot: action note not saved", "error", err)
	}
}

// ── Validation ─────────────────────────────────────────────────────────────

// ActionResult : issue d'une carte.
type ActionResult struct {
	Status     string `json:"status"` // done | failed | rejected
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
	// Undo : carte inverse, prête à être validée (ex. redémarrer après un arrêt).
	Undo *View `json:"undo,omitempty"`
}

// actionError : refus avant exécution (introuvable, déjà traitée…).
type actionError struct {
	status int
	code   string
	msg    string
}

func (e *actionError) Error() string { return e.msg }

func (h *Controller) person(c *gin.Context) (user uuid.UUID, session uuid.UUID, acr string, err error) {
	user, okUser := security.InternalUserID(c)
	session, acr, okSession := security.PersonSession(c)
	if !okUser || !okSession {
		return uuid.Nil, uuid.Nil, "", &actionError{http.StatusForbidden, "COPILOT_PERSON_ONLY", "only a signed-in person can validate an action"}
	}
	return user, session, acr, nil
}

func writeActionError(c *gin.Context, err error) {
	var ae *actionError
	if errors.As(err, &ae) {
		jsonError(c, ae.status, ae.code, ae.msg)
		return
	}
	jsonError(c, http.StatusServiceUnavailable, "COPILOT_UNAVAILABLE", "the Copilot is temporarily unavailable")
}

// take : charge la carte de la personne et la réserve (une seule issue).
func (h *Controller) take(c *gin.Context, token, userID string) (*Proposal, error) {
	s := h.Service
	ctx := c.Request.Context()
	p, err := s.proposals().load(ctx, token, userID)
	switch {
	case errors.Is(err, errProposalNotFound):
		return nil, &actionError{http.StatusGone, "ACTION_EXPIRED", "this card has expired or does not exist"}
	case err != nil:
		return nil, err
	}
	if err := s.proposals().claim(ctx, token); err != nil {
		if errors.Is(err, errProposalHandled) {
			actionsTotal.WithLabelValues(p.Request.Tool, "conflict").Inc()
			return nil, &actionError{http.StatusConflict, "ACTION_ALREADY_HANDLED", "this card has already been handled"}
		}
		return nil, err
	}
	return p, nil
}

// Approve : la personne valide la carte ; l'action est jouée, une fois, avec
// un jeton délégué en écriture de courte durée. Un échec n'est jamais
// rejoué automatiquement (l'état réel est à vérifier).
func (h *Controller) Approve(c *gin.Context) {
	s := h.Service
	if !s.Enabled() {
		jsonError(c, http.StatusServiceUnavailable, "COPILOT_DISABLED", "the Copilot is not available")
		return
	}
	user, session, acr, err := h.person(c)
	if err != nil {
		writeActionError(c, err)
		return
	}
	userID := user.String()
	p, err := h.take(c, c.Param("token"), userID)
	if err != nil {
		writeActionError(c, err)
		return
	}
	ctx := c.Request.Context()
	defer s.proposals().drop(context.WithoutCancel(ctx), p.Token)

	token, _, err := s.STS.MintDelegated(sts.Delegation{UserID: user, SessionID: session, ACR: acr,
		Actor: sts.ActorCopilot, Scope: sts.ScopeWrite, TTL: 2 * time.Minute})
	if err != nil {
		s.Logger.Errorw("copilot: delegated write token", "error", err)
		writeActionError(c, err)
		return
	}
	s.mu.RLock()
	cat, handler := s.catalog, s.handler
	s.mu.RUnlock()
	inv := tools.Invoker{Handler: handler, Token: token, ClientIP: c.ClientIP(), AcceptLanguage: c.GetHeader("Accept-Language")}

	res := cat.Execute(ctx, inv, &p.Request, p.Token)
	out := ActionResult{Status: "done", HTTPStatus: res.Status}
	outcome := "approved"
	if res.IsError {
		out.Status, outcome = "failed", "failed"
		out.Message = errorMessage(res.Content)
	}
	actionsTotal.WithLabelValues(p.Request.Tool, outcome).Inc()

	s.note(context.WithoutCancel(ctx), userID, p.ConversationID,
		fmt.Sprintf("[The person validated the action %s on %q: %s (HTTP %d).]", p.Request.Tool, p.Target, out.Status, res.Status))

	if !res.IsError {
		if inverse := cat.Inverse(&p.Request); inverse != nil {
			if v, err := h.saveUndo(ctx, p, inverse, cat); err == nil {
				out.Undo = v
			}
		}
	}
	c.JSON(http.StatusOK, out)
}

// saveUndo : carte inverse d'une action faite, valable UndoTTL.
func (h *Controller) saveUndo(ctx context.Context, done *Proposal, inverse *tools.Prepared, cat *tools.Catalog) (*View, error) {
	t, ok := cat.Lookup(inverse.Tool)
	if !ok {
		return nil, errors.New("unknown inverse")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := h.Service.now().UTC()
	p := &Proposal{Token: token, UserID: done.UserID, ConversationID: done.ConversationID, Request: *inverse,
		Risk: t.Risk, Target: done.Target, CreatedAt: now, ExpiresAt: now.Add(UndoTTL), UndoOf: done.Token}
	if err := h.Service.proposals().save(ctx, p, UndoTTL); err != nil {
		return nil, err
	}
	v := p.view(t.PathParams())
	return &v, nil
}

// Reject : la personne refuse la carte ; rien n'est fait.
func (h *Controller) Reject(c *gin.Context) {
	s := h.Service
	user, _, _, err := h.person(c)
	if err != nil {
		writeActionError(c, err)
		return
	}
	userID := user.String()
	p, err := h.take(c, c.Param("token"), userID)
	if err != nil {
		writeActionError(c, err)
		return
	}
	ctx := c.Request.Context()
	s.proposals().drop(ctx, p.Token)
	actionsTotal.WithLabelValues(p.Request.Tool, "rejected").Inc()
	s.note(context.WithoutCancel(ctx), userID, p.ConversationID,
		fmt.Sprintf("[The person declined the action %s on %q. Nothing was done.]", p.Request.Tool, p.Target))
	c.JSON(http.StatusOK, ActionResult{Status: "rejected"})
}

func errorMessage(content string) string {
	var e struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal([]byte(content), &e)
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}
