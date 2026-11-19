package router

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

type accessKind int

const (
	accessPublic accessKind = iota + 1
	accessAuthenticated
	accessPermission
	accessClusterAgent
)

// Access : qui peut appeler une route. Argument obligatoire de AddRoute :
// une route ne peut pas être déclarée sans contrôle d'accès explicite, et une
// valeur vide fait échouer le démarrage (registerGroup).
type Access struct {
	kind accessKind
	// delegable : route Authenticated() ouverte aux jetons délégués
	// (sts.MethodDelegated). Faux par défaut : RefuseDelegated s'applique.
	delegable  bool
	permission iam.Permission
	guard      gin.HandlerFunc
	summary    string
}

// Public : sans authentification (login, callbacks OAuth…). Les routes
// publiques sont transmises à l'AuthMiddleware, plus de liste à tenir à jour.
func Public() Access {
	return Access{kind: accessPublic, summary: "publique"}
}

// Authenticated : tout principal authentifié ; le handler ne touche qu'aux
// données de l'appelant (son profil, ses sessions, son organisation personnelle).
func Authenticated() Access {
	return Access{kind: accessAuthenticated, guard: security.RefuseDelegated,
		summary: "tout utilisateur authentifié (données de l'appelant uniquement)"}
}

// AuthenticatedDelegable : comme Authenticated, mais aussi pour un agent de
// betorun (Copilot) agissant au nom de la personne. À réserver aux lectures
// dont le résultat est déjà filtré par les droits de l'appelant (ex. GET
// /projects) : la route ne doit rien modifier du compte.
func AuthenticatedDelegable() Access {
	return Access{kind: accessAuthenticated, delegable: true,
		summary: "tout utilisateur authentifié, ou le Copilot en son nom (données de l'appelant uniquement)"}
}

// guards : fabrique les accès qui ont besoin du middleware de sécurité.
type guards struct {
	mw *security.MiddlewareManager
}

func newGuards(app *App) guards {
	return guards{mw: app.Security.Middleware}
}

// Perm : permission IAM sur la ressource donnée par resolve.
func (g guards) Perm(p iam.Permission, resolve security.ResourceResolver) Access {
	return Access{kind: accessPermission, permission: p, guard: g.mw.Require(p, resolve),
		summary: "permission IAM `" + p.String() + "`"}
}

// PermAll : toutes ces permissions sur la même ressource (ex. dupliquer :
// créer dans le projet et modifier les variables de la source).
func (g guards) PermAll(resolve security.ResourceResolver, perms ...iam.Permission) Access {
	checks := make([]gin.HandlerFunc, 0, len(perms))
	names := make([]string, 0, len(perms))
	for _, p := range perms {
		checks = append(checks, g.mw.Require(p, resolve))
		names = append(names, p.String())
	}
	summary := "permissions IAM `" + strings.Join(names, "`, `") + "`"
	return Access{kind: accessPermission, permission: perms[0], summary: summary, guard: func(c *gin.Context) {
		for _, check := range checks {
			if check(c); c.IsAborted() {
				return
			}
		}
	}}
}

// PermFor : la permission dépend de la ressource (ex. getIamPolicy d'une
// organisation ou d'un projet).
func (g guards) PermFor(permFor func(iam.ResourceName) (iam.Permission, error), resolve security.ResourceResolver) Access {
	return Access{kind: accessPermission, guard: g.mw.RequireFor(permFor, resolve),
		summary: "permission IAM `*.getIamPolicy` / `*.setIamPolicy` selon le type de ressource"}
}

// ClusterAgent : watcher d'un cluster : compte de service système du cluster
// visé par la requête, avec platform.clusters.report (iam.md §7.4).
func (g guards) ClusterAgent(ref security.ClusterRef) Access {
	return Access{kind: accessClusterAgent, permission: iam.PlatformClustersReport,
		guard:   g.mw.RequireClusterAgent(ref),
		summary: "agent du cluster visé (compte de service système), permission IAM `platform.clusters.report`"}
}
