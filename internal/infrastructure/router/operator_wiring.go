package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wI2L/fizz"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	"github.com/thekrauss/kubemanager/internal/modules/operatoraccess"
)

// Accès à l'Opérateur (ADR 0016 §3) : la console obtient un jeton délégué
// de la personne, l'Opérateur demande qui elle est.

func (a *App) newOperatorAccessController() *operatoraccess.Controller {
	if a.STS == nil || a.IAM == nil {
		return nil
	}
	c := &operatoraccess.Controller{STS: a.STS, IAM: a.IAM}
	if a.DB != nil {
		c.Cards = &operatoraccess.Cards{DB: a.DB}
		cards := c.Cards
		a.goBackground("operator-cards-reaper", func(ctx context.Context) {
			cards.RunReaper(ctx, func(err error) { a.Logger.Errorw("operator cards reaper", "error", err) })
		})
	}
	if a.Config != nil && len(a.Config.IAM.OperatorJournalKeys) > 0 {
		keys, err := opjournal.ParsePublicKeys(a.Config.IAM.OperatorJournalKeys)
		if err != nil {
			// Clé mal formée : route fermée, l'Opérateur garde son journal
			// et réessaiera ; le démarrage de l'API n'en dépend pas.
			a.Logger.Errorw("operator journal keys ignored", "error", err)
		} else {
			c.Journal = &operatoraccess.JournalImporter{DB: a.DB, Keys: keys}
		}
	}
	return c
}

func addOperatorRoutes(app *App) {
	r := app.Controllers.OperatorAccess
	if r == nil {
		return
	}
	g := newGuards(app)
	admin := RootGroup.NewGroup("/admin/operator", "Admin — Opérateur").Internal()
	admin.AddRoute("/token", http.MethodPost, "Jeton délégué (15 min, lecture) à présenter à l'Opérateur ; la personne elle-même seulement",
		g.Perm(iam.PlatformOperatorUse, security.PlatformResource), tonic.Handler(r.Token, http.StatusOK))
	// Appelée par l'Opérateur avec ce jeton : la garde est dans le handler
	// (jeton délégué à l'Opérateur exigé, permission vérifiée à chaque appel).
	admin.AddRoute("/me", http.MethodGet, "La personne derrière un jeton de l'Opérateur, et les domaines qui lui sont ouverts",
		AuthenticatedDelegable(), tonic.Handler(r.Me, http.StatusOK))
	// Outils du chemin normal (operator_decisions.go) : même garde que /me.
	admin.AddRoute("/tools", http.MethodGet, "Outils de lecture ouverts à la personne derrière un jeton de l'Opérateur",
		AuthenticatedDelegable(), tonic.Handler(r.Tools, http.StatusOK))
	// Recopie du journal du serveur de gestion dans l'audit : signée par
	// l'Opérateur (Ed25519), sans session ; elle n'ajoute que des lignes
	// d'audit, une seule fois chacune.
	admin.AddRoute("/journal", http.MethodPost, "Recopie du journal de l'Opérateur dans l'audit (signature Ed25519 X-Operator-Signature)",
		Public(), r.Journal.Journal)
	admin.AddRoute("/tools/:name", http.MethodPost, "Appel d'un outil de l'Opérateur, rejoué avec le jeton délégué et le motif du dossier",
		AuthenticatedDelegable(), tonic.Handler(r.Call, http.StatusOK))
	// Cartes (ADR 0016 §3 bis) : l'Opérateur propose et relit l'issue avec
	// son jeton délégué ; la personne valide avec SA session, ici.
	admin.AddRoute("/cards", http.MethodPost, "Proposition d'une action par l'Opérateur : requête contrôlée et figée, rien n'est exécuté",
		AuthenticatedDelegable(), tonic.Handler(r.Propose, http.StatusCreated))
	admin.AddRoute("/cards/:cardID", http.MethodGet, "État d'une carte de l'Opérateur et issue de l'action",
		AuthenticatedDelegable(), tonic.Handler(r.Card, http.StatusOK))
	admin.AddRoute("/cards/:cardID/approve", http.MethodPost, "Validation d'une carte par la personne (sa session) : l'action est jouée une fois ; carte sensible : double authentification",
		g.Perm(iam.PlatformOperatorUse, security.PlatformResource), r.Approve)
	admin.AddRoute("/cards/:cardID/reject", http.MethodPost, "Refus d'une carte, par la personne ou relayé par l'Opérateur",
		AuthenticatedDelegable(), tonic.Handler(r.Reject, http.StatusOK))
}

// bindOperator : une fois toutes les routes enregistrées, le catalogue des
// outils /admin, construit depuis la spécification complète (routes
// internes comprises, en français).
func (a *App) bindOperator(f *fizz.Fizz, h http.Handler) error {
	r := a.Controllers.OperatorAccess
	if r == nil {
		return nil
	}
	spec, err := json.Marshal(f.Generator().API())
	if err != nil {
		return err
	}
	routes, perms := OperatorRoutes(AllGroups)
	cat, err := tools.Build(routes, spec)
	if err != nil {
		return fmt.Errorf("operator: outils : %w", err)
	}
	r.Bind(&operatoraccess.Catalog{Tools: cat, Perms: perms, Handler: h})
	return nil
}
