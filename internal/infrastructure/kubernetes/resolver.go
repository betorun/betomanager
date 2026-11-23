package kubernetes

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Socle multi-cluster (lot 2b) : chaque projet vit sur un cluster ; tout
// accès Kubernetes lié à un projet passe par le client de son cluster.

// Resolver : client du cluster d'un projet (ou d'un namespace de projet,
// unique sur toute la plateforme). Le cluster par défaut est celui de la
// configuration du déploiement.
type Resolver interface {
	ForProject(ctx context.Context, project uuid.UUID) (*ProviderK8s, error)
	ForNamespace(ctx context.Context, namespace string) (*ProviderK8s, error)
	ForCluster(ctx context.Context, clusterID string) (*ProviderK8s, error)
}

// FromConfig : jeu de clients d'un rest.Config (cluster du registre).
func FromConfig(clusterID string, config *rest.Config) (*ProviderK8s, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s clientset: %w", err)
	}
	mClient, err := metricsv1.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("metrics clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &ProviderK8s{ClusterID: clusterID, Client: clientset, Config: config, MetricsClient: mClient, Dynamic: dyn}, nil
}

// Single : un seul cluster (tests, installation sans registre).
type Single struct{ P *ProviderK8s }

func (s Single) ForProject(context.Context, uuid.UUID) (*ProviderK8s, error) { return s.P, nil }
func (s Single) ForNamespace(context.Context, string) (*ProviderK8s, error)  { return s.P, nil }
func (s Single) ForCluster(context.Context, string) (*ProviderK8s, error)    { return s.P, nil }

// Settings : réglages du cluster d'un projet (lot 2c) ; champ vide : celui
// de la configuration du déploiement.
type Settings struct {
	ClusterID        string
	IngressClass     string
	IngressNamespace string
	// IngressTarget : adresse IPv4 ou nom DNS du point d'entrée du cluster.
	IngressTarget string
	StorageClass  string
}

// SettingsResolver : réglages du cluster qui porte un projet.
type SettingsResolver interface {
	SettingsForProject(ctx context.Context, project uuid.UUID) (Settings, error)
}
