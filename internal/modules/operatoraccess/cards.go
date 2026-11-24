package operatoraccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Cartes de l'Opérateur sur les routes /admin (ADR 0016 §3 bis).
//
// L'Opérateur PROPOSE avec son jeton délégué en lecture : l'API contrôle
// l'outil et ses arguments, lit le nom de la cible, et fige la requête dans
// sa base. La personne VALIDE auprès de l'API elle-même, avec sa session :
// carte ordinaire jouée avec un jeton délégué en écriture de deux minutes,
// émis ici et jamais rendu ; carte sensible jouée avec la session de la
// personne (double authentification exigée par la route). L'Opérateur ne
// fait que relire l'issue : il n'a jamais de quoi écrire.

// CardTTL : délai de validation (comme les cartes de l'Opérateur).
const CardTTL = 30 * time.Minute

// cardResultLimit : réponse de la route gardée avec l'issue.
const cardResultLimit = 8 << 10

// cardExecTimeout : durée maximale de l'exécution d'une carte validée
// (celle du jeton délégué en écriture).
const cardExecTimeout = 2 * time.Minute

// cardInterrupted : issue d'une carte restée « en cours » (API arrêtée
// pendant l'action) ; l'état réel est à vérifier.
const cardInterrupted = "interrupted: the API stopped while the action ran; check the actual state"

// Cards : stockage des cartes ; nil = routes fermées.
type Cards struct {
	DB  *gorm.DB
	Now func() time.Time
}

// ReapExecuting : cartes restées « en cours » au-delà du délai d'exécution
// (API arrêtée pendant l'action) : en échec, l'état réel à vérifier.
func (s *Cards) ReapExecuting(ctx context.Context, after time.Duration) (int64, error) {
	now := s.now()
	res := s.DB.WithContext(ctx).Exec(`UPDATE operator_cards SET status='failed', message=?, executed_at=?
		WHERE status='executing' AND decided_at < ?`, cardInterrupted, now, now.Add(-after))
	return res.RowsAffected, res.Error
}

// RunReaper : ReapExecuting au démarrage puis chaque minute.
func (s *Cards) RunReaper(ctx context.Context, logErr func(error)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := s.ReapExecuting(ctx, cardExecTimeout+time.Minute); err != nil && ctx.Err() == nil && logErr != nil {
			logErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Cards) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

type cardRow struct {
	ID              uuid.UUID
	Tool            string
	Request         []byte
	Target          string
	Risk            string
	Strong          bool
	ProposedBy      uuid.UUID
	InvestigationID *uuid.UUID
	Justification   string
	Status          string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DecidedBy       *uuid.UUID
	DecidedEmail    string
	DecidedAt       *time.Time
	HTTPStatus      int
	Message         string
	Result          string
	ExecutedAt      *time.Time
}

// CardView : une carte vue par l'Opérateur et la console.
type CardView struct {
	ID         uuid.UUID      `json:"id"`
	Tool       string         `json:"tool"`
	Summary    string         `json:"summary"`
	Target     string         `json:"target"`
	Details    map[string]any `json:"details,omitempty"`
	Risk       string         `json:"risk"`
	Strong     bool           `json:"strong"`
	Status     string         `json:"status"` // proposed | executing | executed | failed | rejected | expired
	ExpiresAt  time.Time      `json:"expires_at"`
	DecidedBy  string         `json:"decided_by,omitempty"`
	DecidedAt  *time.Time     `json:"decided_at,omitempty"`
	HTTPStatus int            `json:"http_status,omitempty"`
	Message    string         `json:"message,omitempty"`
	Result     string         `json:"result,omitempty"`
}

func (h *Controller) view(r cardRow, cat *Catalog, now time.Time) CardView {
	var p tools.Prepared
	_ = json.Unmarshal(r.Request, &p)
	v := CardView{ID: r.ID, Tool: r.Tool, Target: r.Target, Risk: r.Risk, Strong: r.Strong, Status: r.Status,
		ExpiresAt: r.ExpiresAt, DecidedBy: r.DecidedEmail, DecidedAt: r.DecidedAt, HTTPStatus: r.HTTPStatus,
		Message: r.Message, Result: r.Result}
	if v.Status == "proposed" && !now.Before(r.ExpiresAt) {
		v.Status = "expired"
	}
	var path map[string]bool
	if cat != nil {
		if t, ok := cat.Tools.Lookup(r.Tool); ok {
			v.Summary = firstLine(t.Description)
			path = t.PathParams()
		}
	}
	for k, a := range p.Args {
		if !path[k] {
			if v.Details == nil {
				v.Details = map[string]any{}
			}
			v.Details[k] = a
		}
	}
	return v
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ProposeInput : proposition de l'agent.
type ProposeInput struct {
	Tool            string          `json:"tool" validate:"required" description:"Carte du catalogue (GET /admin/operator/tools, cards)"`
	Arguments       json.RawMessage `json:"arguments" description:"Arguments (objet JSON, selon le schéma de la carte)"`
	InvestigationID *uuid.UUID      `json:"investigation_id" description:"Dossier de l'Opérateur"`
}

func (h *Controller) cardsReady() (*Catalog, error) {
	if h.Cards == nil || h.Cards.DB == nil {
		return nil, betoerrors.New(betoerrors.CodeServiceUnavailable, "the Operator cards are not available")
	}
	return h.catalog()
}

// hasPerms : la personne a ces permissions (droits à jour).
func (h *Controller) hasPerms(ctx context.Context, p iam.Principal, perms ...iam.Permission) (map[iam.Permission]bool, error) {
	granted, err := h.IAM.TestPermissions(ctx, p, iam.Platform, append(perms, iam.PlatformOperatorUse))
	if err != nil {
		return nil, err
	}
	has := map[iam.Permission]bool{}
	for _, g := range granted {
		has[g] = true
	}
	if !has[iam.PlatformOperatorUse] {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "access to the Operator has been withdrawn")
	}
	return has, nil
}

// Propose : l'Opérateur fige une action ; rien n'est exécuté.
func (h *Controller) Propose(c *gin.Context, in *ProposeInput) (*CardView, error) {
	p, err := operatorCaller(c)
	if err != nil {
		return nil, err
	}
	cat, err := h.cardsReady()
	if err != nil {
		return nil, err
	}
	t, ok := cat.Tools.Lookup(in.Tool)
	perm, known := cat.Perms[in.Tool]
	if !ok || !known || !t.Write {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "unknown Operator card")
	}
	has, err := h.hasPerms(c.Request.Context(), p, perm)
	if err != nil {
		return nil, err
	}
	if !has[perm] {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "this person cannot perform this action")
	}
	prepared, err := cat.Tools.Prepare(in.Tool, in.Arguments)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	}
	token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	justification := c.GetHeader(security.HeaderJustification)
	target, err := h.target(c, cat, t, prepared, token, justification)
	if err != nil {
		return nil, err
	}
	user, _ := security.InternalUserID(c)
	raw, _ := json.Marshal(prepared)
	now := h.Cards.now()
	row := cardRow{ID: uuid.New(), Tool: t.Name, Request: raw, Target: target, Risk: string(t.Risk), Strong: t.Strong,
		ProposedBy: user, InvestigationID: in.InvestigationID, Justification: justification, Status: "proposed",
		CreatedAt: now, ExpiresAt: now.Add(CardTTL)}
	if err := h.Cards.DB.WithContext(c.Request.Context()).Exec(`INSERT INTO operator_cards
		(id, tool, request, target, risk, strong, proposed_by, investigation_id, justification, status, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.Tool, raw, row.Target, row.Risk, row.Strong, row.ProposedBy,
		row.InvestigationID, row.Justification, row.Status, row.CreatedAt, row.ExpiresAt).Error; err != nil {
		return nil, err
	}
	v := h.view(row, cat, now)
	return &v, nil
}

// target : nom lisible de la cible, lu avec les droits de la personne (une
// cible introuvable ou interdite : pas de carte).
func (h *Controller) target(c *gin.Context, cat *Catalog, t *tools.Tool, p *tools.Prepared, token, justification string) (string, error) {
	if t.TargetTool == "" {
		return "", nil
	}
	rd, ok := cat.Tools.Lookup(t.TargetTool)
	if !ok {
		return "", nil
	}
	args := map[string]any{}
	for name := range rd.PathParams() {
		args[name] = p.Args[name]
	}
	raw, _ := json.Marshal(args)
	res := cat.Tools.Call(c.Request.Context(), tools.Invoker{Handler: cat.Handler, Token: token, ClientIP: c.ClientIP(),
		AcceptLanguage: c.GetHeader("Accept-Language"), Justification: justification}, t.TargetTool, raw)
	if res.IsError {
		return "", betoerrors.New(betoerrors.CodeNotFound, "the target of this action was not found or is not accessible")
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(res.Content), &v)
	for _, k := range []string{"name", "display_name", "subject", "title", "hostname", "email"} {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), nil
		}
	}
	return "", nil
}

// CardInput : une carte.
type CardInput struct {
	CardID uuid.UUID `path:"cardID" description:"Carte"`
}

func (h *Controller) load(ctx context.Context, id uuid.UUID) (cardRow, error) {
	var rows []cardRow
	if err := h.Cards.DB.WithContext(ctx).Raw(`SELECT * FROM operator_cards WHERE id = ?`, id).Scan(&rows).Error; err != nil {
		return cardRow{}, err
	}
	if len(rows) == 0 {
		return cardRow{}, betoerrors.New(betoerrors.CodeNotFound, "Operator card not found")
	}
	return rows[0], nil
}

// Card : l'état d'une carte (l'Opérateur relit l'issue).
func (h *Controller) Card(c *gin.Context, in *CardInput) (*CardView, error) {
	p, err := operatorCaller(c)
	if err != nil {
		return nil, err
	}
	cat, err := h.cardsReady()
	if err != nil {
		return nil, err
	}
	if _, err := h.hasPerms(c.Request.Context(), p); err != nil {
		return nil, err
	}
	row, err := h.load(c.Request.Context(), in.CardID)
	if err != nil {
		return nil, err
	}
	v := h.view(row, cat, h.Cards.now())
	return &v, nil
}

// ApproveInput : validation par la personne.
type ApproveInput struct {
	CardID uuid.UUID `path:"cardID" description:"Carte"`
}

// Approve : la personne valide, avec SA session ; l'action est jouée une
// fois. Une confirmation d'identité manquante (carte sensible) laisse la
// carte validable : la console la demande puis rejoue la validation.
func (h *Controller) Approve(c *gin.Context) {
	cat, err := h.cardsReady()
	if err != nil {
		writeCardError(c, err)
		return
	}
	user, okUser := security.InternalUserID(c)
	session, acr, okSession := security.PersonSession(c)
	if !okUser || !okSession {
		writeCardError(c, betoerrors.New(betoerrors.CodeForbidden, "only the signed-in person can validate an Operator card"))
		return
	}
	id, err := uuid.Parse(c.Param("cardID"))
	if err != nil {
		writeCardError(c, betoerrors.New(betoerrors.CodeNotFound, "Operator card not found"))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(c.Request.Body, 4<<10))
	ctx := c.Request.Context()
	// Droit de la personne sur l'action, avant de réserver : un refus ne
	// consomme pas la carte (un collègue qui a le droit peut la valider).
	current, err := h.load(ctx, id)
	if err != nil {
		writeCardError(c, err)
		return
	}
	principal, _ := security.PrincipalFromContext(c)
	perm, known := cat.Perms[current.Tool]
	has, err := h.hasPerms(ctx, principal, perm)
	if err != nil {
		writeCardError(c, err)
		return
	}
	if !known || !has[perm] {
		writeCardError(c, betoerrors.New(betoerrors.CodeForbidden, "you do not have the permission this action requires"))
		return
	}
	now := h.Cards.now()
	email := c.GetString("email")

	// Réservation : une seule validation passe, avant l'expiration.
	var rows []cardRow
	if err := h.Cards.DB.WithContext(ctx).Raw(`UPDATE operator_cards SET status='executing', decided_by=?, decided_email=?, decided_at=?
		WHERE id=? AND status='proposed' AND expires_at > ? RETURNING *`, user, email, now, id, now).Scan(&rows).Error; err != nil {
		writeCardError(c, err)
		return
	}
	if len(rows) == 0 {
		if _, err := h.load(ctx, id); err != nil {
			writeCardError(c, err)
			return
		}
		writeCardError(c, betoerrors.New(betoerrors.CodeConflict, "this card has already been decided or has expired"))
		return
	}
	row := rows[0]
	var req tools.Prepared
	if err := json.Unmarshal(row.Request, &req); err != nil {
		writeCardError(c, err)
		return
	}
	token := ""
	if row.Strong {
		// Session de la personne : jamais un jeton délégué pour une carte sensible.
		token = strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	} else {
		token, _, err = h.STS.MintDelegated(sts.Delegation{UserID: user, SessionID: session, ACR: acr,
			Actor: sts.ActorOperator, Scope: sts.ScopeWrite, TTL: 2 * time.Minute})
		if err != nil {
			h.release(ctx, id)
			writeCardError(c, betoerrors.Wrap(err, betoerrors.CodeInternal, "the action could not be authorized"))
			return
		}
	}
	// Détachée de la requête : une page fermée n'interrompt pas l'action.
	exec, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardExecTimeout)
	defer cancel()
	res := cat.Tools.Execute(exec, tools.Invoker{Handler: cat.Handler, Token: token, ClientIP: c.ClientIP(),
		AcceptLanguage: c.GetHeader("Accept-Language"), Justification: row.Justification}, &req, row.ID.String())
	if len(res.StepUp) > 0 {
		// Rien n'a été fait : la carte redevient validable.
		h.release(ctx, id)
		c.Data(http.StatusUnauthorized, "application/json; charset=utf-8", res.StepUp)
		return
	}
	status, message, result := "executed", "", ""
	if res.IsError {
		status, message = "failed", errorMessage(res.Content)
	} else if len(res.Content) <= cardResultLimit {
		result = res.Content
	}
	done := h.Cards.now()
	save := h.Cards.DB.WithContext(context.WithoutCancel(ctx))
	if err := save.Exec(`UPDATE operator_cards SET status=?, http_status=?, message=?, result=?, executed_at=? WHERE id=? AND status='executing'`,
		status, res.Status, message, result, done, id).Error; err != nil {
		writeCardError(c, err)
		return
	}
	row.Status, row.HTTPStatus, row.Message, row.Result, row.ExecutedAt = status, res.Status, message, result, &done
	row.DecidedEmail, row.DecidedAt = email, &now
	c.JSON(http.StatusOK, h.view(row, cat, done))
}

func (h *Controller) release(ctx context.Context, id uuid.UUID) {
	_ = h.Cards.DB.WithContext(context.WithoutCancel(ctx)).Exec(`UPDATE operator_cards SET status='proposed', decided_by=NULL,
		decided_email='', decided_at=NULL WHERE id=? AND status='executing'`, id).Error
}

// RejectInput : refus.
type RejectInput struct {
	CardID uuid.UUID `path:"cardID" description:"Carte"`
	Note   string    `json:"note" validate:"max=500" description:"Raison du refus"`
}

// Reject : refus, par la personne (session) ou relayé par l'Opérateur
// (jeton délégué) : refuser n'écrit rien d'autre que la carte.
func (h *Controller) Reject(c *gin.Context, in *RejectInput) (*CardView, error) {
	cat, err := h.cardsReady()
	if err != nil {
		return nil, err
	}
	p, okP := security.PrincipalFromContext(c)
	user, okUser := security.InternalUserID(c)
	_, _, person := security.PersonSession(c)
	actor, _, delegated := security.Delegation(c)
	if !okP || !okUser || !(person || (delegated && actor == sts.ActorOperator)) {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "only a person or the Operator can reject an Operator card")
	}
	if _, err := h.hasPerms(c.Request.Context(), p); err != nil {
		return nil, err
	}
	now := h.Cards.now()
	var rows []cardRow
	if err := h.Cards.DB.WithContext(c.Request.Context()).Raw(`UPDATE operator_cards SET status='rejected', decided_by=?, decided_email=?,
		decided_at=?, message=? WHERE id=? AND status='proposed' RETURNING *`, user, c.GetString("email"), now,
		strings.TrimSpace(in.Note), in.CardID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if _, err := h.load(c.Request.Context(), in.CardID); err != nil {
			return nil, err
		}
		return nil, betoerrors.New(betoerrors.CodeConflict, "this card has already been decided")
	}
	v := h.view(rows[0], cat, now)
	return &v, nil
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
	if e.Code != "" {
		return e.Code
	}
	if len(content) > 300 {
		content = content[:300]
	}
	return content
}

// writeCardError : erreur de la validation (handler gin brut, pour pouvoir
// rendre tel quel le corps STEP_UP_REQUIRED).
func writeCardError(c *gin.Context, err error) {
	_ = c.Error(err)
	status, body := betoerrors.ToHTTPError(err)
	c.Data(status, "application/json; charset=utf-8", body)
	c.Abort()
}
