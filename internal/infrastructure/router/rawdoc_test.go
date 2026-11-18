package router

import (
	"testing"

	"github.com/loopfz/gadgeto/tonic"
)

// Toute route publique non tonic (flux, envoi de fichiers) doit être décrite
// par Documented : sinon elle disparaît du spec public, de la console, de la
// CLI et des outils du Copilot, sans que rien ne le signale.
func TestRawPublicRoutesAreDocumented(t *testing.T) {
	documentedAPI(t)
	for _, g := range AllGroups {
		visitRoutes(g, false, func(_ *RouteGroup, r *Route, internal bool) {
			if internal || r.Access.kind == accessPublic {
				return
			}
			if _, err := tonic.GetRouteByHandler(r.Handler); err == nil {
				return
			}
			if r.Raw == nil {
				t.Errorf("%s %s : route brute sans Documented(RawDoc{…})", r.Method, r.Path)
			}
		})
	}
}
