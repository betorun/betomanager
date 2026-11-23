package router

import (
	"net/http"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/legal"
	legalRepo "github.com/thekrauss/kubemanager/internal/modules/legal/repository"
	legalSvc "github.com/thekrauss/kubemanager/internal/modules/legal/service"
)

// Conditions de vente et politique d'usage (docs/modules/legal.md). Les
// autres modules passent par legal/contract.TermsGate, que legalService
// satisfait (ex. compute : RequireAccepted avant une commande de VPS).

// legalService : nil sans base.
func (a *App) legalService() *legalSvc.Service {
	if a.DB == nil {
		return nil
	}
	return &legalSvc.Service{Store: &legalRepo.GormStore{DB: a.DB}}
}

func (a *App) newLegalController() *legal.Controller {
	s := a.legalService()
	if s == nil {
		return nil
	}
	return &legal.Controller{Service: s}
}

// addLegalRoutes : lecture publique, acceptation par l'organisation, rédaction (équipe).
func addLegalRoutes(app *App) {
	r := app.Controllers.Legal
	if r == nil {
		return
	}
	g := newGuards(app)
	pub := RootGroup.NewGroup("/legal", "Conditions").
		Named("Conditions", "Conditions générales de vente et politique d'usage acceptable : textes publiés et acceptation par l'organisation.")
	pub.AddRoute("/:kind", http.MethodGet, "Version en vigueur d'un document (ou une version publiée plus ancienne), en français ou en anglais",
		Public(), tonic.Handler(r.Read, http.StatusOK))

	org := RootGroup.NewGroup("/organizations/:id/legal", "Conditions").
		Named("Conditions", "Conditions générales de vente et politique d'usage acceptable : textes publiés et acceptation par l'organisation.")
	org.AddRoute("", http.MethodGet, "Documents en vigueur et versions acceptées par l'organisation",
		g.Perm(iam.OrganizationsGet, security.OrganizationParam("id")), tonic.Handler(r.OrgStatus, http.StatusOK))
	// Accepter engage l'organisation, comme un paiement.
	org.AddRoute("/accept", http.MethodPost, "Accepter la version en vigueur d'un document au nom de l'organisation",
		g.Perm(iam.BillingPaymentsCreate, security.OrganizationParam("id")), tonic.Handler(r.Accept, http.StatusOK))

	admin := RootGroup.NewGroup("/admin/legal", "Admin — conditions").Internal()
	manage := g.Perm(iam.PlatformLegalManage, security.PlatformResource)
	admin.AddRoute("", http.MethodGet, "Documents : version en vigueur, brouillon en cours, acceptations", manage, tonic.Handler(r.Overview, http.StatusOK))
	admin.AddRoute("/:kind/versions", http.MethodGet, "Versions d'un document, avec le nombre d'organisations qui ont accepté chacune", manage, tonic.Handler(r.Versions, http.StatusOK))
	admin.AddRoute("/:kind/versions", http.MethodPost, "Nouveau brouillon (sans texte : copie de la version en vigueur)", manage, tonic.Handler(r.CreateDraft, http.StatusCreated))
	admin.AddRoute("/:kind/versions/:version", http.MethodGet, "Une version complète (textes français et anglais)", manage, tonic.Handler(r.Version, http.StatusOK))
	admin.AddRoute("/:kind/versions/:version", http.MethodPut, "Modifier un brouillon", manage, tonic.Handler(r.UpdateDraft, http.StatusOK))
	admin.AddRoute("/:kind/versions/:version", http.MethodDelete, "Abandonner un brouillon", manage, tonic.Handler(r.DeleteDraft, http.StatusNoContent))
	admin.AddRoute("/:kind/versions/:version/publish", http.MethodPost, "Publier un brouillon : il entre en vigueur et doit être accepté de nouveau",
		manage, tonic.Handler(r.Publish, http.StatusOK))
}
