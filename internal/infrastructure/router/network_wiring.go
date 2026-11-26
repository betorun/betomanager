package router

import (
	"net/http"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	databasesRepo "github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	"github.com/thekrauss/kubemanager/internal/modules/network"
	networkSvc "github.com/thekrauss/kubemanager/internal/modules/network/service"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
)

// newNetworkController : réseau privé d'un projet, lu dans la base (quel
// que soit le système qui applique la politique sur le cluster).
func (a *App) newNetworkController() *network.Controller {
	if a.Repos == nil || a.Repos.Workload == nil {
		return nil
	}
	s := &networkSvc.Service{
		Workloads: a.Repos.Workload,
		Domains:   workloadsRepo.NewDomainStore(a.DB),
		Databases: &databasesRepo.GormRepository{DB: a.DB},
		DomainTLS: !a.Config.Kubernetes.CustomDomainInsecure,
	}
	if v := a.vpcService(); v != nil {
		s.VPC = v // VPS rattachés au réseau privé (ADR 0019)
	}
	return &network.Controller{Service: s}
}

func addNetworkRoutes(app *App) {
	ctrl := app.Controllers.Network
	if ctrl == nil {
		return
	}
	g := newGuards(app)
	RootGroup.NewGroup("/projects/:id/network", "Réseau du projet").
		Named("Réseau", "Réseau privé du projet : qui peut joindre qui, adresses internes et publiques.").
		AddRoute("", http.MethodGet,
			"Réseau privé du projet : conteneurs et bases, leurs adresses internes et publiques, liens déclarés (bases branchées, attentes au démarrage) et règles appliquées",
			g.PermAll(security.ProjectParam("id"), iam.WorkloadsList, iam.DatabasesList),
			tonic.Handler(ctrl.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_project_network"})
}
