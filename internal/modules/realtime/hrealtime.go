package realtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/iam"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/middleware"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

// TTLs Centrifugo (pattern Centrifugo : tokens courts + refresh côté SDK).
const (
	connectionTokenTTL   = 1 * time.Hour
	subscriptionTokenTTL = 10 * time.Minute
)

// Schémas des URL de Centrifugo (http → ws, https → wss).
const (
	schemeHTTP  = "http://"
	schemeHTTPS = "https://"
	schemeWS    = "ws://"
	schemeWSS   = "wss://"
)

// IRealtimeController : contrat HTTP.
type IRealtimeController interface {
	GetConnectionToken(c *gin.Context) (*ConnectionTokenResponse, error)
	GetSubscriptionToken(c *gin.Context, in *SubscribeRequest) (*SubscriptionTokenResponse, error)
}

// Controller — thin, pas de Service dédié (logique 100% dans le handler).
// Authorizer : point de décision IAM (implémenté par *iam.Engine).
type Authorizer interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

type Controller struct {
	signer realtime.TokenSigner
	authz  Authorizer
	wsURL  string // URL WS publique (ex: ws://localhost:8000/connection/websocket)
	// TicketOrganization : organisation d'un ticket de support (canaux
	// support:ticket:<id>) ; nil : ces canaux sont refusés.
	TicketOrganization func(ctx context.Context, ticket uuid.UUID) (uuid.UUID, error)
}

func NewController(cfg configs.CentrifugoConfig, signer realtime.TokenSigner, authz Authorizer) *Controller {
	return &Controller{
		signer: signer,
		authz:  authz,
		wsURL:  buildWSURL(cfg.URL),
	}
}

// Compile-time check.
var _ IRealtimeController = (*Controller)(nil)

//  DTOs

type ConnectionTokenResponse struct {
	Token     string    `json:"token"`
	WSURL     string    `json:"ws_url"`
	Subject   string    `json:"subject"`
	ExpiresAt time.Time `json:"expires_at"`
}

type SubscribeRequest struct {
	Channel string `query:"channel" binding:"required" description:"Canal d'événements (ex: project:xxx:workload:yyy:deploy)"`
}

type SubscriptionTokenResponse struct {
	Token     string    `json:"token"`
	Channel   string    `json:"channel"`
	ExpiresAt time.Time `json:"expires_at"`
}

//  Handlers

// GetConnectionToken : JWT minimal (sub + exp) pour ouvrir la connexion WS.
// N'autorise à AUCUN canal — il faut un subscription token par canal (plus safe).
func (ctrl *Controller) GetConnectionToken(c *gin.Context) (*ConnectionTokenResponse, error) {
	userID, err := requireUserID(c)
	if err != nil {
		return nil, err
	}

	exp := time.Now().Add(connectionTokenTTL)
	token, err := ctrl.signer.GenerateConnectionToken(c.Request.Context(), userID, exp, map[string]any{
		"email": c.GetString(middleware.ContextKeyEmail),
	})
	if err != nil {
		return nil, fmt.Errorf("sign connection token: %w", err)
	}

	return &ConnectionTokenResponse{
		Token:     token,
		WSURL:     ctrl.wsURL,
		Subject:   userID,
		ExpiresAt: exp,
	}, nil
}

// GetSubscriptionToken : JWT scopé à un canal précis. Authz via membership projet.
//
// Extrait le projectID du canal (format `project:<projectID>:...`) et vérifie
// que le user est bien membre du projet. Toute autre forme de canal → 400.
func (ctrl *Controller) GetSubscriptionToken(c *gin.Context, in *SubscribeRequest) (*SubscriptionTokenResponse, error) {
	userID, err := requireUserID(c)
	if err != nil {
		return nil, err
	}
	channel := strings.TrimSpace(in.Channel)
	if channel == "" {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "channel is required")
	}

	// Authz : le principal IAM (identifiant interne, pas celui de l'annuaire
	// porté par « userID ») doit pouvoir lire la ressource du canal.
	principal, ok := security.PrincipalFromContext(c)
	if !ok {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "not a member of this project")
	}
	if ticket, isTicket, err := parseSupportTicketChannel(channel); isTicket {
		if err != nil {
			return nil, err
		}
		if err := ctrl.requireTicketRead(c.Request.Context(), principal, ticket); err != nil {
			return nil, err
		}
		return ctrl.SubscriptionToken(c.Request.Context(), userID, channel)
	}

	projectID, err := parseProjectIDFromChannel(channel)
	if err != nil {
		return nil, err
	}
	if err := ctrl.requireProjectMembership(c.Request.Context(), principal, projectID); err != nil {
		return nil, err
	}
	// Canal d'un VPS (objet complet : adresses, motif d'abus, secours) : il faut
	// aussi pouvoir lire les VPS du projet.
	if isVPSChannel(channel) {
		if err := ctrl.require(c.Request.Context(), principal, iam.VPSGet, iam.ProjectName(projectID)); err != nil {
			return nil, err
		}
	}

	return ctrl.SubscriptionToken(c.Request.Context(), userID, channel)
}

// SubscriptionToken : jeton signé pour un canal déjà autorisé par l'appelant.
func (ctrl *Controller) SubscriptionToken(ctx context.Context, userID, channel string) (*SubscriptionTokenResponse, error) {
	exp := time.Now().Add(subscriptionTokenTTL)
	token, err := ctrl.signer.GenerateSubscriptionToken(ctx, userID, channel, exp)
	if err != nil {
		return nil, fmt.Errorf("sign subscription token: %w", err)
	}
	return &SubscriptionTokenResponse{
		Token:     token,
		Channel:   channel,
		ExpiresAt: exp,
	}, nil
}

// StaffTicketPath : ticket suivi en direct par l'équipe.
type StaffTicketPath struct {
	TicketID string `path:"ticketID" binding:"required" description:"Identifiant du ticket"`
}

// GetStaffTicketToken : jeton du canal d'un ticket pour l'équipe (back-office).
// La route porte la garde staff du ticket (motif, accès journalisé).
func (ctrl *Controller) GetStaffTicketToken(c *gin.Context, in *StaffTicketPath) (*SubscriptionTokenResponse, error) {
	userID, err := requireUserID(c)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(strings.TrimSpace(in.TicketID))
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid ticket id")
	}
	return ctrl.SubscriptionToken(c.Request.Context(), userID, realtime.SupportTicketChannel(id.String()))
}

// parseSupportTicketChannel : "support:ticket:<id>" ; isTicket faux pour
// tout autre espace de canaux.
func parseSupportTicketChannel(channel string) (uuid.UUID, bool, error) {
	parts := strings.Split(channel, realtime.ChannelSeparator)
	if parts[0] != realtime.ChannelSupportNamespace {
		return uuid.Nil, false, nil
	}
	if len(parts) != 3 || parts[1] != "ticket" {
		return uuid.Nil, true, betoerrors.New(betoerrors.CodeInvalidInput, "channel must be 'support:ticket:<ticketID>'")
	}
	id, err := uuid.Parse(strings.TrimSpace(parts[2]))
	if err != nil {
		return uuid.Nil, true, betoerrors.New(betoerrors.CodeInvalidInput, "invalid ticketID in channel")
	}
	return id, true, nil
}

// requireTicketRead : lire les tickets de l'organisation du ticket. Ticket
// inconnu : même refus (son existence n'est pas révélée).
func (ctrl *Controller) requireTicketRead(ctx context.Context, principal iam.Principal, ticket uuid.UUID) error {
	if ctrl.TicketOrganization == nil {
		return betoerrors.New(betoerrors.CodeForbidden, "not allowed to follow this channel")
	}
	org, err := ctrl.TicketOrganization(ctx, ticket)
	if err != nil {
		return betoerrors.New(betoerrors.CodeForbidden, "not allowed to follow this channel")
	}
	return ctrl.require(ctx, principal, iam.SupportTicketsGet, iam.OrganizationName(org))
}

//  Helpers ─

func requireUserID(c *gin.Context) (string, error) {
	uid := strings.TrimSpace(c.GetString(middleware.ContextKeyUserID))
	if uid == "" {
		return "", betoerrors.New(betoerrors.CodeUnauthorized, "authenticated user context missing")
	}
	return uid, nil
}

// parseProjectIDFromChannel : attend "project:<projectID>:...".
// Accepte tous les sous-canaux (events, workload:X, workload:X:deploy).
func parseProjectIDFromChannel(channel string) (uuid.UUID, error) {
	parts := strings.SplitN(channel, realtime.ChannelSeparator, 3)
	if len(parts) < 2 || parts[0] != realtime.ChannelProjectNamespace {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "channel must start with 'project:<projectID>'")
	}
	pid, err := uuid.Parse(strings.TrimSpace(parts[1]))
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid projectID in channel")
	}
	return pid, nil
}

// isVPSChannel : "project:<id>:vps:<id>".
func isVPSChannel(channel string) bool {
	parts := strings.Split(channel, realtime.ChannelSeparator)
	return len(parts) >= 3 && parts[2] == "vps"
}

// require : permission sur la ressource, refus uniforme.
func (ctrl *Controller) require(ctx context.Context, principal iam.Principal, perm iam.Permission, r iam.ResourceName) error {
	decision, err := ctrl.authz.Check(ctx, principal, perm, r)
	if errors.Is(err, iam.ErrResourceNotFound) || (err == nil && !decision.Allowed) {
		return betoerrors.New(betoerrors.CodeForbidden, "not allowed to follow this channel")
	}
	if err != nil {
		return fmt.Errorf("authorization check: %w", err)
	}
	return nil
}

// requireProjectMembership : s'abonner aux événements d'un projet exige
// resourcemanager.projects.get dessus. Réponse uniforme "not a member" : ne
// révèle pas si le projet existe.
func (ctrl *Controller) requireProjectMembership(ctx context.Context, principal iam.Principal, projectID uuid.UUID) error {
	decision, err := ctrl.authz.Check(ctx, principal, iam.ProjectsGet, iam.ProjectName(projectID))
	if errors.Is(err, iam.ErrResourceNotFound) || (err == nil && !decision.Allowed) {
		return betoerrors.New(betoerrors.CodeForbidden, "not a member of this project")
	}
	if err != nil {
		return fmt.Errorf("authorization check: %w", err)
	}
	return nil
}

// buildWSURL : transforme l'URL HTTP configurée en URL WebSocket.
// Ex: http://centrifugo:8000 → ws://centrifugo:8000/connection/websocket
//
//	https://rt.example.com → wss://rt.example.com/connection/websocket
//
// Le frontend qui consomme cet endpoint tourne dans le browser, donc on ne peut
// pas lui donner "ws://centrifugo:8000" (DNS docker interne). L'API devrait
// idéalement être configurée avec l'URL publique — ex: wss://realtime.example.com.
// Pour dev on accepte le http://localhost:8000.
func buildWSURL(httpURL string) string {
	httpURL = strings.TrimRight(strings.TrimSpace(httpURL), "/")
	if httpURL == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(httpURL, schemeHTTPS):
		return schemeWSS + strings.TrimPrefix(httpURL, schemeHTTPS) + realtime.WebSocketPath
	case strings.HasPrefix(httpURL, schemeHTTP):
		return schemeWS + strings.TrimPrefix(httpURL, schemeHTTP) + realtime.WebSocketPath
	default:
		return httpURL + realtime.WebSocketPath
	}
}
