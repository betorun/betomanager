package security

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
)

/*
Jetons délégués (sts.MethodDelegated) : un agent de betorun (le Copilot)
agit au nom d'une personne. Trois bornes, en plus des droits IAM de la
personne, qui restent la limite haute :

 1. jamais d'action sensible (iam.PermissionInfo.Sensitive) : la personne
    la fait elle-même, avec sa double authentification ;
 2. portée « read » : permissions de lecture seulement ;
 3. routes Authenticated() (données de l'appelant : sessions, clés,
    profil, création de projet…) refusées, sauf celles déclarées
    AuthenticatedDelegable().

AuthMethodDelegated : valeur de "authMethod" (et de la colonne auth_method
de l'audit) pour un jeton délégué.
*/
const AuthMethodDelegated = "delegated"

const (
	ctxActor           = "delegationActor"
	ctxDelegationScope = "delegationScope"
)

// Delegation : agent et portée si la requête vient d'un jeton délégué.
func Delegation(c *gin.Context) (actor, scope string, ok bool) {
	if c.GetString("authMethod") != AuthMethodDelegated {
		return "", "", false
	}
	return c.GetString(ctxActor), c.GetString(ctxDelegationScope), true
}

func refuseDelegated(c *gin.Context, code, msg string, perm iam.Permission) {
	c.Set(ctxIAMDecision, audit.DecisionDeny)
	audit.Annotate(c.Request.Context(), "reason", code)
	body := gin.H{"code": code, "error": msg, "message": msg}
	if perm != "" {
		body["permission"] = perm.String()
	}
	c.AbortWithStatusJSON(http.StatusForbidden, body)
}

// checkDelegated : true si la requête peut continuer (pas de délégation, ou
// délégation qui couvre perm) ; sinon la réponse est écrite.
func checkDelegated(c *gin.Context, perm iam.Permission) bool {
	_, scope, ok := Delegation(c)
	if !ok {
		return true
	}
	if info, _ := iam.Info(perm); info.Sensitive {
		refuseDelegated(c, "ACTION_REQUIRES_PERSON",
			"this action must be done by the person themselves, with two-factor authentication", perm)
		return false
	}
	if scope != sts.ScopeWrite && !perm.ReadOnly() {
		refuseDelegated(c, "DELEGATION_SCOPE", "this delegated token is read-only", perm)
		return false
	}
	return true
}

// RefuseDelegated : garde des routes Authenticated() : un jeton délégué n'y
// a pas accès (elles touchent au compte même de la personne).
func RefuseDelegated(c *gin.Context) {
	if _, _, ok := Delegation(c); ok {
		refuseDelegated(c, "ACTION_REQUIRES_PERSON", "this action must be done by the person themselves", "")
		return
	}
	c.Next()
}
