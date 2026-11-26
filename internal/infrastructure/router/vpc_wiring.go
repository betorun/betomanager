package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	clustersRepo "github.com/thekrauss/kubemanager/internal/modules/clusters/repository"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	computeRepo "github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/vpc"
	vpcDomain "github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
	vpcRepo "github.com/thekrauss/kubemanager/internal/modules/vpc/repository"
	vpcSvc "github.com/thekrauss/kubemanager/internal/modules/vpc/service"
)

// vpcClusters : cluster d'un projet, avec son hébergeur et sa région.
type vpcClusters struct {
	store     *clustersRepo.RegistryStore
	defaultID string
}

func (c vpcClusters) ProjectCluster(ctx context.Context, project uuid.UUID) (vpcSvc.Cluster, error) {
	id, _, err := c.store.ProjectCluster(ctx, project)
	if err != nil {
		return vpcSvc.Cluster{}, err
	}
	if id == "" {
		id = c.defaultID
	}
	out := vpcSvc.Cluster{ID: id}
	if cl, err := c.store.Get(ctx, id); err == nil && cl != nil {
		out.Provider, out.Region = cl.Provider, cl.Region
	}
	return out, nil
}

// vpcService : réseau privé des projets étendu à leurs VPS (ADR 0019).
// Lot 1 : aucune mise en œuvre branchée ; rattacher répond VPC_UNAVAILABLE
// jusqu'au lot 2 (tunnel) et à Proxmox (réseau natif).
func (a *App) vpcService() *vpcSvc.Service {
	if a.vpc != nil {
		return a.vpc
	}
	if a.DB == nil {
		return nil
	}
	a.vpc = &vpcSvc.Service{
		Store:    &vpcRepo.Store{DB: a.DB},
		Machines: &computeRepo.GormStore{DB: a.DB},
		Clusters: vpcClusters{store: &clustersRepo.RegistryStore{DB: a.DB}, defaultID: a.clusterRegistry().DefaultID},
		Backends: []vpcDomain.VPCBackend{},
		RegionOf: func(v *computeDomain.VPS) string { return computeDomain.LocationRegion(v.Location) },
	}
	return a.vpc
}

func (a *App) newVPCController() *vpc.Controller {
	s := a.vpcService()
	if s == nil {
		return nil
	}
	return &vpc.Controller{Service: s}
}

func addVPCRoutes(app *App) {
	ctrl := app.Controllers.VPC
	if ctrl == nil {
		return
	}
	g := newGuards(app)
	RootGroup.NewGroup("/projects/:id/network/vpc", "Réseau privé (VPC)").
		Named("Réseau privé (VPC)", "Le réseau privé du projet étendu à ses VPS : adresses privées fixes, noms en .internal.").
		AddRoute("", http.MethodGet, "Réseau privé (VPC) du projet : plage, VPS rattachés, leur adresse privée, leur nom et l'état du lien",
			g.Perm(iam.VPSGet, security.ProjectParam("id")), tonic.Handler(ctrl.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_project_vpc"})

	// Rattacher et détacher : droit réseau du VPS, vérifié sur son projet
	// (seulement si les VPS sont vendus sur ce déploiement).
	if app.Controllers.Compute == nil {
		return
	}
	onVPS := func(p iam.Permission) Access { return g.Perm(p, vpsProject(app.Controllers.Compute)) }
	org := RootGroup.NewGroup("/organizations/:id/compute/vps/:vpsID/vpc", "Réseau privé d'un VPS").
		Named("Réseau privé d'un VPS", "Rattacher un VPS au réseau privé de son projet, ou l'en détacher.")
	org.AddRoute("", http.MethodPost, "Rattacher un VPS au réseau privé de son projet (un jeton d'inscription est rendu une seule fois s'il doit s'inscrire)",
		onVPS(iam.VPSNetwork), tonic.Handler(ctrl.Attach, http.StatusOK))
	org.AddRoute("", http.MethodDelete, "Détacher un VPS du réseau privé : effet immédiat, même VPS injoignable",
		onVPS(iam.VPSNetwork), tonic.Handler(ctrl.Detach, http.StatusNoContent))

	// Inscription par le VPS lui-même : jeton à usage unique, 30 min.
	RootGroup.NewGroup("/vpc/enroll", "Inscription d'un VPS au réseau privé").Internal().
		AddRoute("", http.MethodPost, "Inscription d'un VPS (jeton d'inscription et clé publique générée sur le VPS)",
			Public(), tonic.Handler(ctrl.Enroll, http.StatusOK))
}
