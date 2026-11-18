package router

import (
	"net/http"

	"github.com/loopfz/gadgeto/tonic"
	"k8s.io/client-go/dynamic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
)

// newDatabasesController : bases gérées. Sans accès au cluster, le contrôleur
// existe mais répond « indisponible » (Clusters nil).
func (a *App) newDatabasesController() *databases.Controller {
	s := &databases.Service{Repo: &databases.GormRepository{DB: a.DB}, Projects: a.Repos.Project, Logger: a.Logger}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil && a.K8sProvider.Config != nil {
		dyn, err := dynamic.NewForConfig(a.K8sProvider.Config)
		if err != nil {
			a.Logger.Errorw("databases disabled: dynamic client", "error", err)
		} else {
			s.Clusters = &databases.CNPG{Dynamic: dyn, Kube: a.K8sProvider.Client}
		}
	}
	return databases.NewController(s)
}

func addDatabaseRoutes(app *App) {
	r := app.Controllers.Databases
	if r == nil {
		return
	}
	g := newGuards(app)
	dbs := RootGroup.NewGroup("/projects/:id/databases", "Bases de données").
		Named("Bases de données", "Bases Postgres gérées : une par besoin, joignables seulement depuis les conteneurs du projet.")
	project := security.ProjectParam("id")
	db := security.ProjectChild("id", "databases", "databaseID")

	dbs.AddRoute("", http.MethodGet, "Lister les bases de données du projet", g.Perm(iam.DatabasesList, project),
		tonic.Handler(r.List, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "list_databases"})
	dbs.AddRoute("", http.MethodPost, "Créer une base Postgres (prête en une minute environ)", g.Perm(iam.DatabasesCreate, project),
		tonic.Handler(r.Create, http.StatusCreated))
	dbs.AddRoute("/:databaseID", http.MethodGet, "Détails et état d'une base (jamais le mot de passe)", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_database"})
	dbs.AddRoute("/:databaseID", http.MethodDelete, "Supprimer une base (ses données sont perdues)", g.Perm(iam.DatabasesDelete, db),
		tonic.Handler(r.Delete, http.StatusNoContent))
}
