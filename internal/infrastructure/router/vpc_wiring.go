package router

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	clustersRepo "github.com/thekrauss/kubemanager/internal/modules/clusters/repository"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	computeRepo "github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/vpc"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/backend/tunnel"
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
// Mise en œuvre « tunnel » (lot 2) si l'image de la passerelle et l'URL
// publique de l'API sont configurées ; sinon rattacher répond
// VPC_UNAVAILABLE. Le réseau natif (Proxmox) viendra s'ajouter ici.
func (a *App) vpcService() *vpcSvc.Service {
	if a.vpc != nil {
		return a.vpc
	}
	if a.DB == nil {
		return nil
	}
	store := &vpcRepo.Store{DB: a.DB}
	registry := &clustersRepo.RegistryStore{DB: a.DB}
	a.vpc = &vpcSvc.Service{
		Store:    store,
		Machines: &computeRepo.GormStore{DB: a.DB},
		Clusters: vpcClusters{store: registry, defaultID: a.clusterRegistry().DefaultID},
		Backends: []vpcDomain.VPCBackend{},
		RegionOf: func(v *computeDomain.VPS) string { return computeDomain.LocationRegion(v.Location) },
		APIURL:   a.Config.VPCAPIURL(),
		Logger:   a.Logger,
	}
	if t := a.vpcTunnel(store, registry); t != nil {
		a.vpc.Backends = append(a.vpc.Backends, t)
	}
	return a.vpc
}

// vpcTunnel : passerelle par projet (nil si non configurée). Les clients
// des clusters ne sont ouverts qu'au premier besoin (jamais en mode compute,
// qui ne prépare le réseau qu'en base).
func (a *App) vpcTunnel(store *vpcRepo.Store, registry *clustersRepo.RegistryStore) *tunnel.Backend {
	image := strings.TrimSpace(a.Config.VPC.GatewayImage)
	if image == "" || a.Config.VPCAPIURL() == "" {
		return nil
	}
	var routes []string
	for _, r := range strings.Split(a.Config.VPC.ClusterRoutes, ",") {
		if r = strings.TrimSpace(r); r != "" {
			routes = append(routes, r)
		}
	}
	return &tunnel.Backend{
		Image:  image,
		Routes: routes,
		Kube: func(ctx context.Context, clusterID string) (kubernetes.Interface, error) {
			p, err := a.kube().ForCluster(ctx, clusterID)
			if err != nil {
				return nil, err
			}
			return p.Client, nil
		},
		Namespace: store.Namespace,
		Endpoint: func(ctx context.Context, clusterID string) (string, error) {
			if cl, err := registry.Get(ctx, clusterID); err == nil && cl != nil && cl.IngressTarget != "" {
				return cl.IngressTarget, nil
			}
			return strings.TrimSpace(a.Config.Kubernetes.IngressTarget), nil
		},
	}
}

// startVPCReconcile : réseaux privés alignés sur la base (worker).
func (a *App) startVPCReconcile(ctx context.Context) {
	s := a.vpcService()
	if s == nil || len(s.Backends) == 0 {
		return
	}
	a.goWith(ctx, "vpc-reconcile", func(ctx context.Context) { s.RunReconcile(ctx, vpcSvc.DefaultReconcileEvery) })
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
	// Script d'inscription (sans jeton : donné en argument par la commande
	// rendue au rattachement).
	RootGroup.NewGroup(vpcSvc.EnrollScriptPath, "Script d'inscription d'un VPS au réseau privé").Internal().
		AddRoute("", http.MethodGet, "Script d'inscription d'un VPS au réseau privé (à lancer en root, jeton en argument)",
			Public(), ctrl.Script)
}
