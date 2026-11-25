package router

import (
	"context"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/thekrauss/kubemanager/internal/auth"
	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	clustersRepo "github.com/thekrauss/kubemanager/internal/modules/clusters/repository"
	clustersSvc "github.com/thekrauss/kubemanager/internal/modules/clusters/service"
)

// clusterRegistry : registre des clusters (socle multi-cluster, lot 1). Le
// cluster de la configuration du déploiement est le cluster par défaut, lu
// par le client Kubernetes du process ; les autres, par leur kubeconfig
// déposé dans le back-office (chiffré par la clé maître).
func (a *App) clusterRegistry() *clustersSvc.Registry {
	if a.clusters != nil {
		return a.clusters
	}
	r := &clustersSvc.Registry{
		Store:     &clustersRepo.RegistryStore{DB: a.DB},
		Geo:       &clustersRepo.RegionStore{DB: a.DB},
		DefaultID: strings.TrimSpace(a.Config.Kubernetes.ClusterID),
		Observer:  clustersSvc.MetricsObserver{Logger: a.Logger.With("component", "cluster-health")},
		Logger:    a.Logger.With("component", "cluster-registry"),
		Dial: func(raw []byte) (clustersSvc.Reader, string, error) {
			rc, server, err := clustersRepo.ParseKubeconfig(raw)
			if err != nil {
				return nil, "", err
			}
			k, err := clustersRepo.NewKube(rc)
			if err != nil {
				return nil, "", err
			}
			return k, server, nil
		},
	}
	if s, err := auth.NewSealer(a.Config.IAM.STSKeyEncryptionKey); err == nil {
		r.Sealer = s
	} else {
		a.Logger.Warnw("cluster registry: no master key, kubeconfigs cannot be stored", "error", err)
	}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		r.Default = &clustersRepo.Kube{Client: a.K8sProvider.Client}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.EnsureDefault(ctx); err != nil {
		a.Logger.Warnw("cluster registry: default cluster not registered", "error", err)
	}
	a.clusters = r
	return r
}

// kube : client Kubernetes du cluster de chaque projet (lot 2b) ; sans
// registre utilisable, le cluster de la configuration pour tous.
func (a *App) kube() k8sprovider.Resolver {
	if a.kubeResolver != nil {
		return a.kubeResolver
	}
	reg := a.clusterRegistry()
	a.kubeResolver = &clustersSvc.Pool{
		Registry: reg,
		Projects: &clustersRepo.RegistryStore{DB: a.DB},
		Default:  a.K8sProvider,
		RestConfig: func(raw []byte) (*rest.Config, error) {
			rc, _, err := clustersRepo.ParseKubeconfig(raw)
			return rc, err
		},
	}
	return a.kubeResolver
}

// cnpgClients : clients (dynamique et typé) du cluster du namespace d'un
// projet, pour les bases gérées.
func (a *App) cnpgClients() func(ctx context.Context, ns string) (dynamic.Interface, kubernetes.Interface, error) {
	kube := a.kube()
	return func(ctx context.Context, ns string) (dynamic.Interface, kubernetes.Interface, error) {
		p, err := kube.ForNamespace(ctx, ns)
		if err != nil {
			return nil, nil, err
		}
		dyn := p.Dynamic
		if dyn == nil {
			if dyn, err = dynamic.NewForConfig(p.Config); err != nil {
				return nil, nil, err
			}
		}
		return dyn, p.Client, nil
	}
}

// clusterSettings : réglages (ingress, cible, stockage) du cluster de chaque
// projet (lot 2c) ; nil sans pool.
func (a *App) clusterSettings() *clustersSvc.Pool {
	p, _ := a.kube().(*clustersSvc.Pool)
	return p
}

// storageFor : classe des disques du cluster du namespace d'un projet.
func (a *App) storageFor() func(ctx context.Context, ns string) (string, error) {
	pool := a.clusterSettings()
	if pool == nil {
		return nil
	}
	return func(ctx context.Context, ns string) (string, error) {
		st, err := pool.SettingsForNamespace(ctx, ns)
		return st.StorageClass, err
	}
}

// startClusterHealth : relevé de santé de chaque cluster (worker).
func (a *App) startClusterHealth(ctx context.Context) {
	r := a.clusterRegistry()
	a.goWith(ctx, "cluster-health", func(ctx context.Context) { r.Run(ctx, clustersSvc.DefaultHealthEvery) })
}

// clusterAgents : installation des agents (lot 3) : fichiers de clé émis
// pour le compte système de chaque cluster, accès restreint par le pool.
func (a *App) clusterAgents() *clustersSvc.Agents {
	ag := &clustersSvc.Agents{
		Registry: a.clusterRegistry(),
		Image:    strings.TrimSpace(a.Config.Kubernetes.AgentImage),
		// Agent réseau des IP de sortie (ADR 0014, lot 2).
		EgressImage: strings.TrimSpace(a.Config.Kubernetes.EgressAgentImage),
		Prepare: func(raw []byte) (clustersSvc.AgentInstaller, string, error) {
			rc, server, err := clustersRepo.ParseKubeconfig(raw)
			if err != nil {
				return nil, "", err
			}
			c, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return nil, "", err
			}
			return &clustersRepo.Installer{Client: c}, server, nil
		},
		RestrictedKubeconfig: clustersRepo.RestrictedKubeconfig,
	}
	if u := strings.TrimRight(strings.TrimSpace(a.Config.Builds.PublicAPIURL), "/"); u != "" {
		ag.APIURL = u + "/clusters"
	}
	pool := a.clusterSettings()
	ag.For = func(ctx context.Context, clusterID string) (clustersSvc.AgentInstaller, error) {
		if pool == nil {
			return nil, clustersSvc.ErrNoDefaultCluster
		}
		p, err := pool.ForCluster(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		return &clustersRepo.Installer{Client: p.Client}, nil
	}
	if a.ClusterAgents != nil {
		ag.Keys = clustersSvc.ClusterAgentKeys{Agents: a.ClusterAgents}
	}
	return ag
}
