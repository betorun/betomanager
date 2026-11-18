package security

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
)

// Jetons de session des personnes, émis par betorun (docs/architecture/
// auth.md §4). Ils remplissent le même contexte qu'un jeton de l'annuaire :
// gardes IAM, double authentification (acr, auth_time) et handlers existants
// fonctionnent sans changement.

const ctxSessionID = "sessionID"

// sessionUserTTL : fraîcheur de l'email et de l'identifiant d'annuaire mis
// en cache (l'état du compte, lui, est vérifié par le STS).
const sessionUserTTL = 5 * time.Minute

type sessionUser struct {
	keycloakID string
	email      string
	loaded     time.Time
}

var sessionUsers sync.Map // identifiant interne → sessionUser

func (m *MiddlewareManager) sessionUser(ctx context.Context, id uuid.UUID) (*sessionUser, bool) {
	if v, ok := sessionUsers.Load(id); ok {
		u := v.(*sessionUser)
		if time.Since(u.loaded) < sessionUserTTL {
			return u, true
		}
	}
	if m.authRepo == nil {
		return nil, false
	}
	user, err := m.authRepo.GetUserByID(ctx, id)
	if err != nil || user == nil {
		return nil, false
	}
	u := &sessionUser{keycloakID: user.KeycloakUserID, email: user.Email, loaded: time.Now()}
	sessionUsers.Store(id, u)
	return u, true
}

// applySessionToken : contexte d'une personne authentifiée par une session betorun.
func (m *MiddlewareManager) applySessionToken(c *gin.Context, v *sts.Verified) bool {
	kind, id, err := v.Principal.Parse()
	if err != nil || kind != iam.KindUser {
		return false
	}
	u, ok := m.sessionUser(c.Request.Context(), id)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return true
	}
	c.Set(ctxPrincipal, string(v.Principal))
	c.Set(ctxInternalUserID, id.String())
	c.Set(ctxSessionID, v.SessionID)
	c.Set("userID", u.keycloakID) // usage interne (appels à l'annuaire)
	c.Set("email", u.email)
	c.Set("tokenID", v.TokenID)
	c.Set(ctxAuthAcr, v.ACR)
	if v.Method == sts.MethodDelegated {
		// Un agent au nom de la personne : ses droits, bornés par
		// checkDelegated (jamais d'action sensible) et RefuseDelegated.
		c.Set("authMethod", AuthMethodDelegated)
		c.Set(ctxActor, v.Actor)
		c.Set(ctxDelegationScope, v.Scope)
		c.Next()
		return true
	}
	// « bearer » : une personne ; la double authentification s'applique.
	c.Set("authMethod", "bearer")
	c.Set(ctxAuthTime, v.AuthTime.Unix())
	c.Next()
	return true
}

// SessionID : session betorun de la requête (absente pour un jeton de
// l'ancien fournisseur, une clé API ou une machine).
func SessionID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.GetString(ctxSessionID))
	return id, err == nil
}

// PersonSession : la requête vient-elle d'une personne connectée par une
// session betorun (ni clé API, ni compte de service, ni délégation) ? Rend
// la session et son acr (niveau d'authentification), pour une délégation.
func PersonSession(c *gin.Context) (sessionID uuid.UUID, acr string, ok bool) {
	if c.GetString("authMethod") != "bearer" {
		return uuid.Nil, "", false
	}
	id, ok := SessionID(c)
	if !ok {
		return uuid.Nil, "", false
	}
	return id, c.GetString(ctxAuthAcr), true
}
