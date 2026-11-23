package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/rest"

	k8s "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// Pool : clients Kubernetes par cluster (socle multi-cluster, lot 2b) ;
// implémente kubernetes.Resolver pour les modules.

// ProjectClusters : cluster d'un projet ou d'un namespace de projet, même
// supprimé (sa suppression passe encore par son cluster) ; "" : cluster par
// défaut ; ok false : projet inconnu.
type ProjectClusters interface {
	ProjectCluster(ctx context.Context, project uuid.UUID) (cluster string, ok bool, err error)
	NamespaceCluster(ctx context.Context, namespace string) (cluster string, ok bool, err error)
}

// ErrUnknownProject : aucun projet ne porte cet identifiant ou ce namespace.
var ErrUnknownProject = errors.New("no project for this id or namespace")

// ErrNoDefaultCluster : ni cluster de la configuration ni accès au registre.
var ErrNoDefaultCluster = errors.New("no Kubernetes cluster configured")

// projectTTL : un projet ne change pas de cluster (lot 2) : rattachement gardé.
const projectTTL = 10 * time.Minute

type Pool struct {
	Registry *Registry
	Projects ProjectClusters
	// Default : clients du cluster de la configuration (nil : aucun).
	Default *k8s.ProviderK8s
	// RestConfig : rest.Config d'un kubeconfig déposé (contrôlé comme au dépôt).
	RestConfig func(raw []byte) (*rest.Config, error)
	Now        func() time.Time

	mu       sync.Mutex
	clusters map[string]pooled
	projects map[string]placed
}

type pooled struct {
	setAt time.Time
	p     *k8s.ProviderK8s
}

type placed struct {
	cluster string
	until   time.Time
}

var _ k8s.Resolver = (*Pool)(nil)

func (p *Pool) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pool) ForProject(ctx context.Context, project uuid.UUID) (*k8s.ProviderK8s, error) {
	return p.forKey(ctx, "p:"+project.String(), func() (string, bool, error) { return p.Projects.ProjectCluster(ctx, project) })
}

func (p *Pool) ForNamespace(ctx context.Context, namespace string) (*k8s.ProviderK8s, error) {
	return p.forKey(ctx, "n:"+namespace, func() (string, bool, error) { return p.Projects.NamespaceCluster(ctx, namespace) })
}

func (p *Pool) forKey(ctx context.Context, key string, lookup func() (string, bool, error)) (*k8s.ProviderK8s, error) {
	cluster, err := p.clusterOf(ctx, key, lookup)
	if err != nil {
		return nil, err
	}
	return p.ForCluster(ctx, cluster)
}

// clusterOf : cluster d'un projet ou d'un namespace ("" : défaut), gardé projectTTL.
func (p *Pool) clusterOf(_ context.Context, key string, lookup func() (string, bool, error)) (string, error) {
	p.mu.Lock()
	hit, ok := p.projects[key]
	p.mu.Unlock()
	if !ok || p.now().After(hit.until) {
		cluster, found, err := lookup()
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrUnknownProject
		}
		hit = placed{cluster: cluster, until: p.now().Add(projectTTL)}
		p.mu.Lock()
		if p.projects == nil {
			p.projects = map[string]placed{}
		}
		p.projects[key] = hit
		p.mu.Unlock()
	}
	return hit.cluster, nil
}

// ForCluster : "" ou cluster sans accès déposé qui est celui de la
// configuration : clients du process ; sinon ceux du kubeconfig déposé.
func (p *Pool) ForCluster(ctx context.Context, clusterID string) (*k8s.ProviderK8s, error) {
	if clusterID == "" || (p.Registry != nil && clusterID == p.Registry.DefaultID) {
		c, err := p.cluster(ctx, clusterID)
		if err == nil && c != nil && len(c.KubeconfigCiphertext) > 0 {
			return p.fromRegistry(c)
		}
		if p.Default == nil {
			return nil, ErrNoDefaultCluster
		}
		return p.Default, nil
	}
	c, err := p.cluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if len(c.KubeconfigCiphertext) == 0 {
		return nil, domain.ErrClusterUnreachable
	}
	return p.fromRegistry(c)
}

func (p *Pool) cluster(ctx context.Context, clusterID string) (*domain.Cluster, error) {
	if p.Registry == nil || clusterID == "" {
		return nil, nil
	}
	return p.Registry.Store.Get(ctx, clusterID)
}

func (p *Pool) fromRegistry(c *domain.Cluster) (*k8s.ProviderK8s, error) {
	setAt := time.Time{}
	if c.KubeconfigSetAt != nil {
		setAt = *c.KubeconfigSetAt
	}
	p.mu.Lock()
	hit, ok := p.clusters[c.ClusterID]
	p.mu.Unlock()
	if ok && hit.setAt.Equal(setAt) {
		return hit.p, nil
	}
	if p.Registry.Sealer == nil {
		return nil, ErrNoSealer
	}
	raw, err := p.Registry.Sealer.Open(c.KubeconfigCiphertext, aad(c.ClusterID))
	if err != nil {
		return nil, err
	}
	rc, err := p.RestConfig(raw)
	if err != nil {
		return nil, err
	}
	prov, err := k8s.FromConfig(c.ClusterID, rc)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.clusters == nil {
		p.clusters = map[string]pooled{}
	}
	p.clusters[c.ClusterID] = pooled{setAt: setAt, p: prov}
	p.mu.Unlock()
	return prov, nil
}

var _ k8s.SettingsResolver = (*Pool)(nil)

// SettingsForProject : réglages (ingress, cible, stockage) du cluster du
// projet ; cluster absent du registre : réglages vides (configuration).
func (p *Pool) SettingsForProject(ctx context.Context, project uuid.UUID) (k8s.Settings, error) {
	return p.settings(ctx, "p:"+project.String(), func() (string, bool, error) { return p.Projects.ProjectCluster(ctx, project) })
}

// SettingsForNamespace : idem, par le namespace d'un projet.
func (p *Pool) SettingsForNamespace(ctx context.Context, namespace string) (k8s.Settings, error) {
	return p.settings(ctx, "n:"+namespace, func() (string, bool, error) { return p.Projects.NamespaceCluster(ctx, namespace) })
}

func (p *Pool) settings(ctx context.Context, key string, lookup func() (string, bool, error)) (k8s.Settings, error) {
	cluster, err := p.clusterOf(ctx, key, lookup)
	if err != nil {
		return k8s.Settings{}, err
	}
	if cluster == "" && p.Registry != nil {
		cluster = p.Registry.DefaultID
	}
	out := k8s.Settings{ClusterID: cluster}
	c, err := p.cluster(ctx, cluster)
	switch {
	case errors.Is(err, domain.ErrClusterNotFound):
		return out, nil
	case err != nil:
		return out, err
	case c == nil:
		return out, nil
	}
	out.IngressClass, out.IngressNamespace = c.IngressClass, c.IngressNamespace
	out.IngressTarget, out.StorageClass = c.IngressTarget, c.StorageClass
	return out, nil
}
