package pull

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
)

// one : un seul cluster pour tous les projets.
func one(k kubernetes.Interface) func(context.Context, uuid.UUID) (kubernetes.Interface, error) {
	return func(context.Context, uuid.UUID) (kubernetes.Interface, error) { return k, nil }
}

type fakeAuths []domain.Auth

func (f fakeAuths) Auths(context.Context, uuid.UUID, string) ([]domain.Auth, error) { return f, nil }

func secretHosts(t *testing.T, s *corev1.Secret) map[string]map[string]string {
	t.Helper()
	var cfg struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	require.NoError(t, json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg))
	return cfg.Auths
}

// Un seul Secret : le registre de la plateforme et les registres externes.
func TestSync_PlatformAndExternal(t *testing.T) {
	k8s := fake.NewClientset()
	project := uuid.New()
	s := &Secrets{K8s: one(k8s),
		Platform: &Platform{Host: "registry.betorun.app", Creds: registry.Credentials{Secret: []byte("s")}},
		External: fakeAuths{{Host: "ghcr.io", Username: "moi", Password: "ghp_x"}, {Host: domain.DockerHub, Username: "moi", Password: "dckr"}}}
	require.NoError(t, s.Sync(context.Background(), "km-p", project))

	sec, err := k8s.CoreV1().Secrets("km-p").Get(context.Background(), SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeDockerConfigJson, sec.Type)
	hosts := secretHosts(t, sec)
	assert.Len(t, hosts, 3)
	assert.Equal(t, "ghp_x", hosts["ghcr.io"]["password"])
	assert.Equal(t, "dckr", hosts["https://index.docker.io/v1/"]["password"])
	assert.Contains(t, hosts, "registry.betorun.app")

	// Accès retirés : le Secret est réécrit sans eux.
	s.External = fakeAuths{}
	require.NoError(t, s.Sync(context.Background(), "km-p", project))
	sec, err = k8s.CoreV1().Secrets("km-p").Get(context.Background(), SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Len(t, secretHosts(t, sec), 1)
}

// Plus aucune entrée : le Secret disparaît (aucun accès résiduel).
func TestSync_NothingLeftDeletesSecret(t *testing.T) {
	k8s := fake.NewClientset()
	project := uuid.New()
	s := &Secrets{K8s: one(k8s), External: fakeAuths{{Host: "ghcr.io", Username: "moi", Password: "x"}}}
	require.NoError(t, s.Sync(context.Background(), "km-p", project))

	s.External = fakeAuths{}
	require.ErrorIs(t, s.Sync(context.Background(), "km-p", project), ErrNothingToPull)
	_, err := k8s.CoreV1().Secrets("km-p").Get(context.Background(), SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

// Refresh ne crée rien dans un projet qui n'a encore rien déployé.
func TestRefresh_OnlyExistingSecret(t *testing.T) {
	k8s := fake.NewClientset()
	project := uuid.New()
	s := &Secrets{K8s: one(k8s), External: fakeAuths{{Host: "ghcr.io", Username: "moi", Password: "x"}}}
	require.NoError(t, s.Refresh(context.Background(), "km-p", project))
	_, err := k8s.CoreV1().Secrets("km-p").Get(context.Background(), SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	require.NoError(t, s.Sync(context.Background(), "km-p", project))
	s.External = fakeAuths{}
	require.NoError(t, s.Refresh(context.Background(), "km-p", project), "plus rien : supprimé sans erreur")
	_, err = k8s.CoreV1().Secrets("km-p").Get(context.Background(), SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

// Lot 2b : le Secret est écrit sur le cluster du projet, pas sur un autre.
func TestSync_WritesOnProjectCluster(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	clusters := map[uuid.UUID]*fake.Clientset{a: fake.NewClientset(), b: fake.NewClientset()}
	s := &Secrets{External: fakeAuths{{Host: "ghcr.io", Username: "moi", Password: "x"}},
		K8s: func(_ context.Context, p uuid.UUID) (kubernetes.Interface, error) { return clusters[p], nil }}

	require.NoError(t, s.Sync(context.Background(), "km-b", b))

	_, err := clusters[b].CoreV1().Secrets("km-b").Get(context.Background(), SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = clusters[a].CoreV1().Secrets("km-b").Get(context.Background(), SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "rien sur le cluster de l'autre projet")
}
