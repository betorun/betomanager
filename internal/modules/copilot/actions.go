package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

var actionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "kubemanager", Subsystem: "copilot", Name: "actions_total",
	Help: "Cartes d'action du Copilot, par outil et issue (approved|rejected|failed|conflict).",
}, []string{"tool", "outcome"})

// ── Validation ─────────────────────────────────────────────────────────────

// ActionResult : issue d'une carte.
type ActionResult struct {
	Status     string `json:"status"` // done | failed | rejected
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
	// Undo : carte inverse, prête à être validée (ex. redémarrer après un arrêt).
	Undo *service.View `json:"undo,omitempty"`
	// Result : réponse JSON de la route en cas de succès (ex. le build lancé,
	// que la console suit en direct), bornée.
	Result json.RawMessage `json:"result,omitempty"`
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
func (h *Controller) take(c *gin.Context, token, userID string) (*service.Proposal, error) {
	p, err := h.load(c, token, userID)
	if err != nil {
		return nil, err
	}
	return p, h.claim(c, p)
}

func (h *Controller) load(c *gin.Context, token, userID string) (*service.Proposal, error) {
	p, err := h.Service.ProposalsStore().Load(c.Request.Context(), token, userID)
	switch {
	case errors.Is(err, service.ErrProposalNotFound):
		return nil, &actionError{http.StatusGone, "ACTION_EXPIRED", "this card has expired or does not exist"}
	case err != nil:
		return nil, err
	}
	return p, nil
}

func (h *Controller) claim(c *gin.Context, p *service.Proposal) error {
	if err := h.Service.ProposalsStore().Claim(c.Request.Context(), p.Token); err != nil {
		if errors.Is(err, service.ErrProposalHandled) {
			actionsTotal.WithLabelValues(p.Request.Tool, "conflict").Inc()
			return &actionError{http.StatusConflict, "ACTION_ALREADY_HANDLED", "this card has already been handled"}
		}
		return err
	}
	return nil
}

// ApproveRequest : corps facultatif de la validation.
type ApproveRequest struct {
	// Confirm : nom de la ressource retapé (cartes ConfirmName).
	Confirm string `json:"confirm"`
}

// maxApproveBody : corps de la validation (un nom retapé).
const maxApproveBody = 4 << 10

// Approve : la personne valide la carte ; l'action est jouée, une fois. Carte
// ordinaire : avec un jeton délégué en écriture de courte durée. Carte
// sensible (Strong) : avec la session de la personne elle-même, l'API exige
// alors sa confirmation récente comme dans la console ; si elle manque, la
// carte reste validable et la console demande la double authentification
// (STEP_UP_REQUIRED) puis rejoue la validation. Un échec n'est jamais rejoué
// automatiquement (l'état réel est à vérifier).
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
	var in ApproveRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxApproveBody)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			jsonError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
	}
	userID := user.String()
	p, err := h.load(c, c.Param("token"), userID)
	if err != nil {
		writeActionError(c, err)
		return
	}
	// Nom retapé : vérifié avant toute réservation (une faute de frappe ne
	// consomme pas la carte).
	if p.ConfirmName && strings.TrimSpace(in.Confirm) != strings.TrimSpace(p.Target) {
		jsonError(c, http.StatusBadRequest, "CONFIRMATION_MISMATCH", "the name typed does not match the resource")
		return
	}
	var token string
	if p.Strong {
		// Session de la personne : jamais un jeton délégué pour une carte sensible.
		bearer, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || strings.TrimSpace(bearer) == "" {
			jsonError(c, http.StatusForbidden, "COPILOT_PERSON_ONLY", "this action must be confirmed by the signed-in person")
			return
		}
		token = strings.TrimSpace(bearer)
	}
	if err := h.claim(c, p); err != nil {
		writeActionError(c, err)
		return
	}
	ctx := c.Request.Context()
	if !p.Strong {
		token, _, err = s.STS.MintDelegated(sts.Delegation{UserID: user, SessionID: session, ACR: acr,
			Actor: sts.ActorCopilot, Scope: sts.ScopeWrite, TTL: 2 * time.Minute})
		if err != nil {
			s.ProposalsStore().Release(context.WithoutCancel(ctx), p.Token)
			s.Logger.Errorw("copilot: delegated write token", "error", err)
			writeActionError(c, err)
			return
		}
	}
	s.Mu.RLock()
	cat, handler := s.Catalog, s.Handler
	s.Mu.RUnlock()
	inv := tools.Invoker{Handler: handler, Token: token, ClientIP: c.ClientIP(), AcceptLanguage: c.GetHeader("Accept-Language")}

	res := cat.Execute(ctx, inv, &p.Request, p.Token)
	if len(res.StepUp) > 0 {
		// Rien n'a été fait : la carte redevient validable, la console
		// demande la confirmation d'identité puis rejoue la validation.
		s.ProposalsStore().Release(context.WithoutCancel(ctx), p.Token)
		actionsTotal.WithLabelValues(p.Request.Tool, "step_up").Inc()
		c.Data(http.StatusUnauthorized, "application/json; charset=utf-8", res.StepUp)
		return
	}
	defer s.ProposalsStore().Drop(context.WithoutCancel(ctx), p.Token)

	out := ActionResult{Status: "done", HTTPStatus: res.Status}
	outcome := "approved"
	if res.IsError {
		out.Status, outcome = "failed", "failed"
		out.Message = errorMessage(res.Content)
	} else if len(res.Content) <= resultLimit && json.Valid([]byte(res.Content)) {
		out.Result = json.RawMessage(res.Content)
	}
	actionsTotal.WithLabelValues(p.Request.Tool, outcome).Inc()

	how := ""
	if p.Strong {
		how = " with their two-factor confirmation"
	}
	note := fmt.Sprintf("[The person validated%s the action %s on %q: %s (HTTP %d)", how, p.Request.Tool, p.Target, out.Status, res.Status)
	if id := resultID(out.Result); id != "" {
		note += ", resource id " + id // pour les outils de lecture du tour suivant
	}
	s.Note(context.WithoutCancel(ctx), userID, p.ConversationID, note+".]")

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
func (h *Controller) saveUndo(ctx context.Context, done *service.Proposal, inverse *tools.Prepared, cat *tools.Catalog) (*service.View, error) {
	t, ok := cat.Lookup(inverse.Tool)
	if !ok {
		return nil, errors.New("unknown inverse")
	}
	token, err := service.NewToken()
	if err != nil {
		return nil, err
	}
	now := h.Service.CurrentTime().UTC()
	p := &service.Proposal{Token: token, UserID: done.UserID, ConversationID: done.ConversationID, Request: *inverse,
		Risk: t.Risk, Target: done.Target, CreatedAt: now, ExpiresAt: now.Add(service.UndoTTL), UndoOf: done.Token}
	if err := h.Service.ProposalsStore().Save(ctx, p, service.UndoTTL); err != nil {
		return nil, err
	}
	v := p.View(t.PathParams())
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
	s.ProposalsStore().Drop(ctx, p.Token)
	actionsTotal.WithLabelValues(p.Request.Tool, "rejected").Inc()
	s.Note(context.WithoutCancel(ctx), userID, p.ConversationID,
		fmt.Sprintf("[The person declined the action %s on %q. Nothing was done.]", p.Request.Tool, p.Target))
	c.JSON(http.StatusOK, ActionResult{Status: "rejected"})
}

// resultLimit : au-delà, la réponse n'est pas renvoyée à la console.
const resultLimit = 8 << 10

// resultID : identifiant de la ressource créée (build, conteneur), s'il y en a un.
func resultID(raw json.RawMessage) string {
	var v struct {
		ID         string `json:"id"`
		WorkloadID string `json:"workload_id"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if v.ID != "" {
		return v.ID
	}
	return v.WorkloadID
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
