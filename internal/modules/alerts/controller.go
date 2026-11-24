package alerts

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/repository"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/service"
)

// Controller : préférences d'e-mail de la personne connectée et lien de
// désabonnement.
type Controller struct {
	Prefs *repository.PreferenceStore
	// Key : clé des liens de désabonnement (la même que Notifier.UnsubscribeKey).
	Key []byte
	// Webhooks : webhooks des projets (nil : routes indisponibles).
	Webhooks    *service.WebhookService
	ProjectName func(ctx context.Context, project uuid.UUID) (string, error)
	ConsoleURL  string
}

func prefError(err error) error {
	switch {
	case errors.Is(err, domain.ErrUnknownCategory), errors.Is(err, domain.ErrInvalidLocale), errors.Is(err, domain.ErrInvalidToken):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	}
	return err
}

// GetPreferences : GET /identity/me/preferences.
func (c *Controller) GetPreferences(ctx *gin.Context) (*domain.Preferences, error) {
	id, ok := security.InternalUserID(ctx)
	if !ok {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "authenticated user context missing")
	}
	p, err := c.Prefs.Get(ctx.Request.Context(), id)
	return p, prefError(err)
}

// UpdatePreferences : PATCH /identity/me/preferences.
func (c *Controller) UpdatePreferences(ctx *gin.Context, in *domain.PreferencesUpdate) (*domain.Preferences, error) {
	id, ok := security.InternalUserID(ctx)
	if !ok {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "authenticated user context missing")
	}
	p, err := c.Prefs.Update(ctx.Request.Context(), id, *in)
	return p, prefError(err)
}

// UnsubscribeRequest : jeton du lien reçu.
type UnsubscribeRequest struct {
	Token string `json:"token" form:"token"`
}

// UnsubscribeResponse : catégorie coupée.
type UnsubscribeResponse struct {
	Category string `json:"category"`
}

// Unsubscribe : POST /alerts/unsubscribe. Jeton dans l'URL (désabonnement « un
// clic » des messageries, corps List-Unsubscribe=One-Click) ou dans le corps
// JSON (page de la console). Sans session : le jeton signé suffit.
func (c *Controller) Unsubscribe(ctx *gin.Context) {
	token := ctx.Query("token")
	if token == "" {
		var in UnsubscribeRequest
		if strings.HasPrefix(ctx.ContentType(), "application/json") {
			_ = ctx.ShouldBindJSON(&in)
		} else {
			_ = ctx.ShouldBind(&in)
		}
		token = in.Token
	}
	user, category, err := service.ParseUnsubscribeToken(c.Key, token)
	if err == nil {
		err = c.Prefs.Mute(ctx.Request.Context(), user, category)
	}
	if err != nil {
		// Même format d'erreur que les autres routes (hook de tonic).
		status, body := tonic.GetErrorHook()(ctx, prefError(err))
		ctx.JSON(status, body)
		return
	}
	ctx.JSON(http.StatusOK, UnsubscribeResponse{Category: category})
}

// ── Webhooks d'un projet ─────────────────────────────────────────────────

// WebhookProjectPath : projet visé.
type WebhookProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

// WebhookPath : webhook visé.
type WebhookPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	WebhookID string `path:"webhookID" binding:"required" description:"ID du webhook"`
}

// CreateWebhookInput : projet et webhook.
type CreateWebhookInput struct {
	WebhookProjectPath
	service.CreateWebhookRequest
}

func webhookError(err error) error {
	switch {
	case errors.Is(err, service.ErrWebhookURL), errors.Is(err, service.ErrWebhookKind), errors.Is(err, service.ErrWebhookName),
		errors.Is(err, service.ErrTelegramToken), errors.Is(err, service.ErrTelegramChat), errors.Is(err, service.ErrTelegramHooked):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, service.ErrEmailAddress), errors.Is(err, service.ErrEmailLink):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, service.ErrTelegramFailure), errors.Is(err, service.ErrEmailUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case errors.Is(err, service.ErrWebhookLimit):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	case errors.Is(err, service.ErrWebhookNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	}
	return err
}

func parseIDs(project, webhook string) (uuid.UUID, uuid.UUID, error) {
	p, err := uuid.Parse(project)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	if webhook == "" {
		return p, uuid.Nil, nil
	}
	w, err := uuid.Parse(webhook)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, service.ErrWebhookNotFound.Error())
	}
	return p, w, nil
}

func (c *Controller) webhooksReady() error {
	if c.Webhooks == nil {
		return betoerrors.New(betoerrors.CodeServiceUnavailable, "alert webhooks are not available")
	}
	return nil
}

// ListWebhooks : GET /projects/:id/alert-webhooks.
func (c *Controller) ListWebhooks(ctx *gin.Context, in *WebhookProjectPath) (*service.WebhookList, error) {
	if err := c.webhooksReady(); err != nil {
		return nil, err
	}
	p, _, err := parseIDs(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	return c.Webhooks.List(ctx.Request.Context(), p)
}

// CreateWebhook : POST /projects/:id/alert-webhooks.
func (c *Controller) CreateWebhook(ctx *gin.Context, in *CreateWebhookInput) (*service.CreatedWebhook, error) {
	if err := c.webhooksReady(); err != nil {
		return nil, err
	}
	p, _, err := parseIDs(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	by := ""
	if id, ok := security.InternalUserID(ctx); ok {
		by = "user:" + id.String()
	}
	out, err := c.Webhooks.Create(ctx.Request.Context(), p, by, in.CreateWebhookRequest)
	return out, webhookError(err)
}

// TestWebhook : POST /projects/:id/alert-webhooks/:webhookID/test.
func (c *Controller) TestWebhook(ctx *gin.Context, in *WebhookPath) (*service.Webhook, error) {
	if err := c.webhooksReady(); err != nil {
		return nil, err
	}
	p, w, err := parseIDs(in.ProjectID, in.WebhookID)
	if err != nil {
		return nil, err
	}
	name := ""
	if c.ProjectName != nil {
		name, _ = c.ProjectName(ctx.Request.Context(), p)
	}
	out, err := c.Webhooks.Test(ctx.Request.Context(), p, w, name, c.ConsoleURL)
	return out, webhookError(err)
}

// DeleteWebhook : DELETE /projects/:id/alert-webhooks/:webhookID.
func (c *Controller) DeleteWebhook(ctx *gin.Context, in *WebhookPath) error {
	if err := c.webhooksReady(); err != nil {
		return err
	}
	p, w, err := parseIDs(in.ProjectID, in.WebhookID)
	if err != nil {
		return err
	}
	return webhookError(c.Webhooks.Delete(ctx.Request.Context(), p, w))
}

// TelegramChatsInput : projet et jeton du bot.
type TelegramChatsInput struct {
	WebhookProjectPath
	service.TelegramChatsRequest
}

// TelegramChats : POST /projects/:id/alert-webhooks/telegram/chats. Nom du bot
// et discussions où il a été ajouté ; rien n'est enregistré.
func (c *Controller) TelegramChats(ctx *gin.Context, in *TelegramChatsInput) (*service.TelegramChats, error) {
	if err := c.webhooksReady(); err != nil {
		return nil, err
	}
	if _, _, err := parseIDs(in.ProjectID, ""); err != nil {
		return nil, err
	}
	out, err := c.Webhooks.TelegramChats(ctx.Request.Context(), in.BotToken)
	return out, webhookError(err)
}

// emailLinkToken : jeton dans l'URL (retrait « un clic » des messageries) ou
// dans le corps JSON (page de la console).
func emailLinkToken(ctx *gin.Context) string {
	if t := ctx.Query("token"); t != "" {
		return t
	}
	var in service.EmailLinkRequest
	if strings.HasPrefix(ctx.ContentType(), "application/json") {
		_ = ctx.ShouldBindJSON(&in)
	} else {
		_ = ctx.ShouldBind(&in)
	}
	return in.Token
}

func (c *Controller) emailLink(ctx *gin.Context, run func(context.Context, string) (*service.EmailLinkResult, error)) {
	err := c.webhooksReady()
	var out *service.EmailLinkResult
	if err == nil {
		out, err = run(ctx.Request.Context(), emailLinkToken(ctx))
	}
	if err != nil {
		status, body := tonic.GetErrorHook()(ctx, webhookError(err))
		ctx.JSON(status, body)
		return
	}
	ctx.JSON(http.StatusOK, out)
}

// ConfirmEmail : POST /alerts/email/confirm. Lien reçu par l'adresse ajoutée
// aux alertes d'un projet ; sans session.
func (c *Controller) ConfirmEmail(ctx *gin.Context) {
	c.emailLink(ctx, func(ctx context.Context, t string) (*service.EmailLinkResult, error) { return c.Webhooks.ConfirmEmail(ctx, t) })
}

// RemoveEmail : POST /alerts/email/remove. Lien « ne plus recevoir » d'une
// alerte ; sans session, rejouable.
func (c *Controller) RemoveEmail(ctx *gin.Context) {
	c.emailLink(ctx, func(ctx context.Context, t string) (*service.EmailLinkResult, error) { return c.Webhooks.RemoveEmail(ctx, t) })
}
