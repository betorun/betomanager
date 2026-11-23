package router

import (
	"net/http"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/status"
	statusRepo "github.com/thekrauss/kubemanager/internal/modules/status/repository"
	statusSvc "github.com/thekrauss/kubemanager/internal/modules/status/service"
)

// Page de statut publique (docs/modules/status.md) : sondes lues en base,
// incidents et maintenances annoncés par l'équipe.

func (a *App) newStatusController() *status.Controller {
	if a.DB == nil {
		return nil
	}
	store := &statusRepo.GormStore{DB: a.DB}
	d := a.Config.Domains.OVH
	features := statusSvc.Features{VPS: a.Config.Compute.Enabled, Domains: d.ApplicationKey != "" && d.OrderingEnabled}
	return &status.Controller{Service: &statusSvc.Service{Components: statusSvc.DefaultComponents(store, features), Store: store}}
}

// addStatusRoutes : page publique ; incidents (équipe, /admin).
func addStatusRoutes(app *App) {
	r := app.Controllers.Status
	if r == nil {
		return
	}
	g := newGuards(app)
	pub := RootGroup.NewGroup("/status", "Statut").
		Named("Statut", "État des services de la plateforme, incidents en cours et maintenances programmées.")
	pub.AddRoute("", http.MethodGet, "État des services, incidents des 7 derniers jours et maintenances à venir (mis à jour toutes les 30 secondes)",
		Public(), tonic.Handler(r.Public, http.StatusOK))

	admin := RootGroup.NewGroup("/admin/status", "Admin — statut").Internal()
	manage := g.Perm(iam.PlatformStatusManage, security.PlatformResource)
	admin.AddRoute("", http.MethodGet, "État des composants, avec la raison donnée par chaque sonde", manage, tonic.Handler(r.Overview, http.StatusOK))
	admin.AddRoute("/incidents", http.MethodGet, "Incidents et maintenances, les plus récents d'abord", manage, tonic.Handler(r.List, http.StatusOK))
	admin.AddRoute("/incidents", http.MethodPost, "Annoncer un incident ou programmer une maintenance", manage, tonic.Handler(r.Create, http.StatusCreated))
	admin.AddRoute("/incidents/:incidentID", http.MethodGet, "Un incident et son fil de messages", manage, tonic.Handler(r.Get, http.StatusOK))
	admin.AddRoute("/incidents/:incidentID", http.MethodPut, "Corriger un incident (textes, composants, impact, fenêtre)", manage, tonic.Handler(r.Edit, http.StatusOK))
	admin.AddRoute("/incidents/:incidentID/updates", http.MethodPost, "Ajouter un message et changer le statut de l'incident", manage, tonic.Handler(r.AddUpdate, http.StatusOK))
	admin.AddRoute("/incidents/:incidentID/resolve", http.MethodPost, "Clore un incident", manage, tonic.Handler(r.Resolve, http.StatusOK))
}
