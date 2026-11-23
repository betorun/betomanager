package router

import (
	"context"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/auth"
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

// startClusterHealth : relevé de santé de chaque cluster (worker).
func (a *App) startClusterHealth(ctx context.Context) {
	r := a.clusterRegistry()
	a.goWith(ctx, "cluster-health", func(ctx context.Context) { r.Run(ctx, clustersSvc.DefaultHealthEvery) })
}
