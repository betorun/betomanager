package router

import (
	"context"
	"net/http"
	"time"

	"github.com/loopfz/gadgeto/tonic"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/egress"
	egressDomain "github.com/thekrauss/kubemanager/internal/modules/egress/domain"
	egressRepo "github.com/thekrauss/kubemanager/internal/modules/egress/repository"
	egressSvc "github.com/thekrauss/kubemanager/internal/modules/egress/service"
)

// egressService : IP de sortie dédiée par projet (ADR 0014). Passerelle
// Cilium, jointe par le client du cluster du projet (pool multi-cluster).
func (a *App) egressService() *egressSvc.Service {
	if a.egress != nil {
		return a.egress
	}
	kube := a.kube()
	cilium := &egressRepo.Cilium{Dynamic: func(ctx context.Context, clusterID string) (dynamic.Interface, error) {
		p, err := kube.ForCluster(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		if p.Dynamic != nil {
			return p.Dynamic, nil
		}
		return dynamic.NewForConfig(p.Config)
	}}
	// Consigne et état des agents réseau, par le client du cluster.
	nodes := &egressRepo.Nodes{Kube: func(ctx context.Context, clusterID string) (kubernetes.Interface, error) {
		p, err := kube.ForCluster(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		return p.Client, nil
	}}
	a.egress = &egressSvc.Service{
		Store:          &egressRepo.Store{DB: a.DB},
		Gateways:       map[string]egressSvc.Gateway{egressDomain.GatewayCilium: cilium},
		Nodes:          nodes,
		DefaultCluster: a.clusterRegistry().DefaultID,
		Logger:         a.Logger.With("component", "egress"),
	}
	return a.egress
}

func (a *App) newEgressController() *egress.Controller {
	if a.DB == nil {
		return nil
	}
	return &egress.Controller{Service: a.egressService()}
}

// startEgressSweep : IP des projets supprimés rendues (worker).
func (a *App) startEgressSweep(ctx context.Context) {
	if a.DB == nil {
		return
	}
	s := a.egressService()
	a.goWith(ctx, "egress-sweep", func(ctx context.Context) { s.Run(ctx, 5*time.Minute) })
}

func addEgressRoutes(app *App) {
	ctrl := app.Controllers.Egress
	if ctrl == nil {
		return
	}
	g := newGuards(app)
	project := security.ProjectParam("id")
	grp := RootGroup.NewGroup("/projects/:id/network/egress", "IP de sortie dédiée").
		Named("IP de sortie dédiée", "Une adresse IP propre au projet, par laquelle sortent tous ses conteneurs (à faire autoriser chez un partenaire).")
	grp.AddRoute("", http.MethodGet, "IP de sortie dédiée du projet, ou si l'on peut en demander une",
		g.Perm(iam.EgressAddressesGet, project), tonic.Handler(ctrl.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_egress_ip"})
	grp.AddRoute("", http.MethodPost, "Demander une IP de sortie dédiée (facturée au mois) : tous les conteneurs du projet sortent par elle",
		g.Perm(iam.EgressAddressesManage, project), tonic.Handler(ctrl.Request, http.StatusCreated))
	grp.AddRoute("", http.MethodDelete, "Rendre l'IP de sortie dédiée : le trafic ressort par une adresse partagée, la facturation s'arrête",
		g.Perm(iam.EgressAddressesManage, project), tonic.Handler(ctrl.Release, http.StatusNoContent))

	admin := RootGroup.NewGroup("/admin/clusters/:clusterID/egress-addresses", "Admin — IP de sortie d'un cluster").Internal()
	admin.AddRoute("", http.MethodGet, "Réserve d'IP de sortie du cluster, avec le projet qui tient chacune",
		g.Perm(iam.PlatformClustersList, security.PlatformResource), tonic.Handler(ctrl.AdminList, http.StatusOK))
	admin.AddRoute("", http.MethodPost, "Ajouter une IP à la réserve (déjà montée sur l'interface du nœud passerelle)",
		g.Perm(iam.PlatformClustersManage, security.PlatformResource), tonic.Handler(ctrl.AdminDeclare, http.StatusCreated))
	admin.AddRoute("/:addressID", http.MethodDelete, "Retirer une IP libre de la réserve",
		g.Perm(iam.PlatformClustersManage, security.PlatformResource), tonic.Handler(ctrl.AdminRemove, http.StatusNoContent))
}
