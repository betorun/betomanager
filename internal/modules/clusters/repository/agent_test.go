package repository

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// tokenController : Kubernetes remplit le jeton d'un Secret de compte de service.
func tokenController(c *kubefake.Clientset) {
	c.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		if s.Type == corev1.SecretTypeServiceAccountToken {
			s.Data = map[string][]byte{corev1.ServiceAccountTokenKey: []byte("tok"), corev1.ServiceAccountRootCAKey: []byte("ca")}
		}
		return false, nil, nil
	})
}

func TestInstaller_PlatformAndAgent(t *testing.T) {
	ctx := context.Background()
	c := kubefake.NewClientset()
	tokenController(c)
	i := &Installer{Client: c}

	token, ca, err := i.Platform(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tok", string(token))
	assert.Equal(t, "ca", string(ca))
	role, err := c.RbacV1().ClusterRoles().Get(ctx, domain.PlatformAccount, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, PlatformRules(), role.Rules)
	// Rejouée : rien ne casse.
	_, _, err = i.Platform(ctx)
	require.NoError(t, err)

	require.NoError(t, i.AgentRBAC(ctx))
	b, err := c.RbacV1().ClusterRoleBindings().Get(ctx, domain.AgentName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.AgentName, b.Subjects[0].Name)

	spec := domain.AgentSpec{ClusterID: "gra-1", Image: "thekrauss/watcher:abc", APIURL: "https://api.example/kmanager/v1/clusters", KeyFile: []byte(`{"k":1}`)}
	require.NoError(t, i.Agent(ctx, spec))
	d, err := c.AppsV1().Deployments(domain.AgentNamespace).Get(ctx, domain.AgentName, metav1.GetOptions{})
	require.NoError(t, err)
	ctr := d.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "thekrauss/watcher:abc", ctr.Image)
	assert.Contains(t, ctr.Args, "--cluster-id=gra-1")
	assert.Contains(t, ctr.Args, "--restate-url=https://api.example/kmanager/v1/clusters")
	assert.Contains(t, ctr.Args, "--managed-ns-label=kubemanager.io/managed")
	assert.True(t, *ctr.SecurityContext.ReadOnlyRootFilesystem)
	sec, err := c.CoreV1().Secrets(domain.AgentNamespace).Get(ctx, domain.AgentKeySecret, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, `{"k":1}`, string(sec.Data["key.json"]))
	hash := d.Spec.Template.Annotations[keyHashAnnotation]
	assert.NotEmpty(t, hash)
	assert.NotContains(t, hash, "k", "empreinte, jamais la clé")

	// Image seule : clé et empreinte gardées (pas de redémarrage pour la clé).
	require.NoError(t, i.Agent(ctx, domain.AgentSpec{ClusterID: "gra-1", Image: "thekrauss/watcher:def", APIURL: spec.APIURL}))
	d, _ = c.AppsV1().Deployments(domain.AgentNamespace).Get(ctx, domain.AgentName, metav1.GetOptions{})
	assert.Equal(t, hash, d.Spec.Template.Annotations[keyHashAnnotation])
	assert.Equal(t, "thekrauss/watcher:def", d.Spec.Template.Spec.Containers[0].Image)
	sec, _ = c.CoreV1().Secrets(domain.AgentNamespace).Get(ctx, domain.AgentKeySecret, metav1.GetOptions{})
	assert.Equal(t, `{"k":1}`, string(sec.Data["key.json"]))

	// Nouvelle clé : nouvelle empreinte.
	require.NoError(t, i.Agent(ctx, domain.AgentSpec{ClusterID: "gra-1", Image: "thekrauss/watcher:def", APIURL: spec.APIURL, KeyFile: []byte(`{"k":2}`)}))
	d, _ = c.AppsV1().Deployments(domain.AgentNamespace).Get(ctx, domain.AgentName, metav1.GetOptions{})
	assert.NotEqual(t, hash, d.Spec.Template.Annotations[keyHashAnnotation])

	st, err := i.State(ctx)
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.True(t, st.ManagedByPlatform)
	assert.Equal(t, int32(1), st.Desired)
}

// Kubeconfig sans droits RBAC : « administrateur requis ».
func TestInstaller_AdminRequired(t *testing.T) {
	c := kubefake.NewClientset()
	tokenController(c)
	c.PrependReactor("*", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "x", nil)
	})
	_, _, err := (&Installer{Client: c}).Platform(context.Background())
	assert.ErrorIs(t, err, domain.ErrAdminRequired)
}

func TestInstaller_StateNotInstalled(t *testing.T) {
	st, err := (&Installer{Client: kubefake.NewClientset()}).State(context.Background())
	require.NoError(t, err)
	assert.False(t, st.Installed)
}

// Le kubeconfig restreint passe les contrôles d'un dépôt.
func TestRestrictedKubeconfig(t *testing.T) {
	raw, err := RestrictedKubeconfig("gra-1", "https://gra-1.example:6443", []byte("tok"), []byte("-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"))
	require.NoError(t, err)
	rc, server, err := ParseKubeconfig(raw)
	require.NoError(t, err)
	assert.Equal(t, "https://gra-1.example:6443", server)
	assert.Equal(t, "tok", rc.BearerToken)
}

// deploy/k8s/rbac.yaml (installation à la main du premier cluster) et le
// rôle posé par la préparation ne divergent jamais.
func TestPlatformRules_MatchDeployManifest(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/k8s/rbac.yaml")
	require.NoError(t, err)
	var found bool
	for _, doc := range strings.Split(string(raw), "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != "ClusterRole" {
			continue
		}
		found = true
		assert.Equal(t, domain.PlatformAccount, role.Name)
		assert.Equal(t, PlatformRules(), role.Rules)
	}
	assert.True(t, found, "ClusterRole absent de deploy/k8s/rbac.yaml")
}

// Sur un vrai cluster, avec l'accès restreint de la plateforme : la
// préparation est refusée (pas administrateur) sans rien changer, et l'état
// de l'agent se lit.
func TestInstaller_LiveRestricted(t *testing.T) {
	path := os.Getenv("CLUSTER_LIVE_KUBECONFIG")
	if path == "" {
		t.Skip("CLUSTER_LIVE_KUBECONFIG not set")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	rc, _, err := ParseKubeconfig(raw)
	require.NoError(t, err)
	c, err := kubernetes.NewForConfig(rc)
	require.NoError(t, err)
	i := &Installer{Client: c}
	ctx := context.Background()
	_, _, err = i.Platform(ctx)
	assert.ErrorIs(t, err, domain.ErrAdminRequired)
	st, err := i.State(ctx)
	require.NoError(t, err)
	t.Logf("agent: %+v", *st)
}
