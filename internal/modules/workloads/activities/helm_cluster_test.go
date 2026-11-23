package activities

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
)

// clusters : résolveur de test, cluster de chaque namespace de projet.
type clusters map[string]*k8sprovider.ProviderK8s

func (c clusters) ForProject(context.Context, uuid.UUID) (*k8sprovider.ProviderK8s, error) {
	return nil, fmt.Errorf("unexpected project lookup")
}
func (c clusters) ForNamespace(_ context.Context, ns string) (*k8sprovider.ProviderK8s, error) {
	if p, ok := c[ns]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown namespace %s", ns)
}
func (c clusters) ForCluster(context.Context, string) (*k8sprovider.ProviderK8s, error) {
	return nil, fmt.Errorf("unexpected cluster lookup")
}

func providerAt(t *testing.T, url string) *k8sprovider.ProviderK8s {
	t.Helper()
	p, err := k8sprovider.FromConfig("", &rest.Config{Host: url})
	require.NoError(t, err)
	return p
}

// fakeAPIServer : juste assez de l'API Kubernetes pour que Helm conclue
// « aucune release » ; compte les appels reçus.
func fakeAPIServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"SecretList","apiVersion":"v1","items":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// Helm vise le cluster de la configuration de la plateforme, jamais celui de
// KUBECONFIG ou de ~/.kube/config : sur un serveur séparé du cluster, il
// échouait ou visait un autre cluster.
func TestHelmUsesConfiguredCluster(t *testing.T) {
	configured, configuredCalls := fakeAPIServer(t)
	other, otherCalls := fakeAPIServer(t)

	kubeconfig := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: autre
  cluster: {server: %q}
contexts:
- name: autre
  context: {cluster: autre, user: u}
current-context: autre
users:
- name: u
  user: {}
`, other.URL)), 0o600))
	t.Setenv("KUBECONFIG", kubeconfig)

	a := &WorkloadActivities{Kube: clusters{"km-x": providerAt(t, configured.URL)}}
	require.NoError(t, a.UninstallChart(context.Background(), "km-x", "web"))

	assert.Positive(t, configuredCalls.Load(), "appels vers le cluster configuré")
	assert.Zero(t, otherCalls.Load(), "aucun appel vers le cluster de KUBECONFIG")
}

// Lot 2b : chaque release part vers le cluster du projet de son namespace.
func TestHelmUsesProjectCluster(t *testing.T) {
	first, firstCalls := fakeAPIServer(t)
	second, secondCalls := fakeAPIServer(t)
	a := &WorkloadActivities{Kube: clusters{"km-a": providerAt(t, first.URL), "km-b": providerAt(t, second.URL)}}

	require.NoError(t, a.UninstallChart(context.Background(), "km-b", "web"))
	assert.Zero(t, firstCalls.Load(), "aucun appel vers le cluster de l'autre projet")
	assert.Positive(t, secondCalls.Load(), "appels vers le cluster du projet")

	require.NoError(t, a.UninstallChart(context.Background(), "km-a", "web"))
	assert.Positive(t, firstCalls.Load())
}
