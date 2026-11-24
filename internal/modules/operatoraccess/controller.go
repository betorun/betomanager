// Package operatoraccess : accès de l'équipe à l'Opérateur (ADR 0016 §3).
//
// L'Opérateur tourne hors de l'API, sur le serveur de gestion. La console
// lui présente un jeton délégué court de la personne (acteur
// betorun:operator, lecture seule) ; l'Opérateur demande ensuite à l'API qui
// est la personne et ce qu'elle peut faire. Il n'a aucun droit propre.
package operatoraccess

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

// TokenTTL : durée du jeton ; la console en redemande un avant la fin.
const TokenTTL = sts.MaxDelegatedTTL

// Minter : émission du jeton délégué (sts.Service).
type Minter interface {
	MintDelegated(d sts.Delegation) (string, time.Time, error)
}

// Authorizer : droits de la personne (iam.Engine).
type Authorizer interface {
	TestPermissions(ctx context.Context, p iam.Principal, r iam.ResourceName, perms []iam.Permission) ([]iam.Permission, error)
}

// Controller : routes /admin/operator.
type Controller struct {
	STS Minter
	IAM Authorizer
	// Journal : recopie du journal de l'Opérateur ; nil = route fermée.
	Journal *JournalImporter
	// Cards : cartes sur les routes /admin ; nil = routes fermées.
	Cards *Cards

	slot catalogSlot
}

// TokenResponse : jeton à présenter à l'Opérateur (Authorization: Bearer).
type TokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Token : jeton délégué de la personne connectée pour l'Opérateur. Une
// personne seulement (ni compte de service, ni autre agent).
func (h *Controller) Token(c *gin.Context) (*TokenResponse, error) {
	user, okUser := security.InternalUserID(c)
	session, acr, okSession := security.PersonSession(c)
	if !okUser || !okSession {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "the Operator is only available to a signed-in person")
	}
	token, exp, err := h.STS.MintDelegated(sts.Delegation{UserID: user, SessionID: session, ACR: acr,
		Actor: sts.ActorOperator, Scope: sts.ScopeRead, TTL: TokenTTL})
	if err != nil {
		return nil, betoerrors.Wrap(err, betoerrors.CodeInternal, "the Operator token could not be issued")
	}
	return &TokenResponse{Token: token, ExpiresAt: exp}, nil
}

// Capabilities : domaines ouverts à la personne dans l'Opérateur.
type Capabilities struct {
	// Infrastructure : chemin de secours (clusters, serveurs) — administrateurs.
	Infrastructure bool `json:"infrastructure"`
	Support        bool `json:"support"`
	Commercial     bool `json:"commercial"`
	Compute        bool `json:"compute"`
}

// MeResponse : la personne derrière un jeton de l'Opérateur.
type MeResponse struct {
	UserID       uuid.UUID    `json:"user_id"`
	Email        string       `json:"email"`
	Capabilities Capabilities `json:"capabilities"`
}

// Me : appelé par l'Opérateur avec le jeton délégué qu'on lui présente.
// Refuse tout autre jeton : une session ou le Copilot n'ouvrent pas
// l'Opérateur.
func (h *Controller) Me(c *gin.Context) (*MeResponse, error) {
	if actor, _, ok := security.Delegation(c); !ok || actor != sts.ActorOperator {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "an Operator token is required")
	}
	user, ok := security.InternalUserID(c)
	principal, okP := security.PrincipalFromContext(c)
	if !ok || !okP {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "an Operator token is required")
	}
	granted, err := h.IAM.TestPermissions(c.Request.Context(), principal, iam.Platform, []iam.Permission{
		iam.PlatformOperatorUse, iam.PlatformClustersManage, iam.PlatformSupportManage,
		iam.PlatformBillingAccountsGet, iam.PlatformComputeGet,
	})
	if err != nil {
		return nil, err
	}
	has := map[iam.Permission]bool{}
	for _, p := range granted {
		has[p] = true
	}
	// Le rôle a pu être retiré depuis l'émission du jeton.
	if !has[iam.PlatformOperatorUse] {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "access to the Operator has been withdrawn")
	}
	return &MeResponse{UserID: user, Email: c.GetString("email"), Capabilities: Capabilities{
		Infrastructure: has[iam.PlatformClustersManage],
		Support:        has[iam.PlatformSupportManage],
		Commercial:     has[iam.PlatformBillingAccountsGet],
		Compute:        has[iam.PlatformComputeGet],
	}}, nil
}
