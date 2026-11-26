package tunnel

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/vpctest"
	"github.com/thekrauss/kubemanager/internal/vpcgateway"
)

const vpsKey = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="

// fakeClusters : un faux cluster par identifiant ; le port du Service est
// alloué à la création, comme le ferait le cluster.
type fakeClusters struct {
	mu   sync.Mutex
	byID map[string]*fake.Clientset
}

func (f *fakeClusters) get(_ context.Context, id string) (kubernetes.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byID == nil {
		f.byID = map[string]*fake.Clientset{}
	}
	if c, ok := f.byID[id]; ok {
		return c, nil
	}
	c := fake.NewClientset()
	c.PrependReactor("create", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Service)
		for i := range s.Spec.Ports {
			s.Spec.Ports[i].NodePort = 31820
		}
		return false, nil, nil
	})
	f.byID[id] = c
	return c, nil
}

func newBackend(clusters *fakeClusters, status func(context.Context, kubernetes.Interface, string) ([]vpcgateway.PeerStatus, error)) *Backend {
	if status == nil {
		status = func(context.Context, kubernetes.Interface, string) ([]vpcgateway.PeerStatus, error) { return nil, nil }
	}
	return &Backend{
		Kube:       clusters.get,
		Namespace:  func(_ context.Context, p uuid.UUID) (string, error) { return "km-" + p.String()[:12], nil },
		Endpoint:   func(context.Context, string) (string, error) { return "198.51.100.10", nil },
		Image:      "registry.example/vpc-gateway:test",
		PeerStatus: status,
	}
}

func TestContract(t *testing.T) {
	vpctest.RunBackendContract(t, func(*testing.T) domain.VPCBackend { return newBackend(&fakeClusters{}, nil) })
}

func vpcAt(cidr string) domain.VPC {
	p := netip.MustParsePrefix(cidr)
	return domain.VPC{ProjectID: uuid.New(), ClusterID: "c1", CIDR: p, Gateway: domain.GatewayOf(p)}
}

func TestHandles(t *testing.T) {
	b := newBackend(&fakeClusters{}, nil)
	assert.True(t, b.Handles(domain.Placement{VPSProvider: "ovh"}))
	assert.False(t, b.Handles(domain.Placement{VPSProvider: "proxmox"}))
	b.Image = ""
	assert.False(t, b.Handles(domain.Placement{VPSProvider: "ovh"}), "sans image : indisponible")
}

func TestGatewayObjects(t *testing.T) {
	ctx := context.Background()
	clusters := &fakeClusters{}
	b := newBackend(clusters, nil)
	v := vpcAt("10.248.7.0/24")
	ns := "km-" + v.ProjectID.String()[:12]
	m := domain.Member{VPSID: uuid.New(), Name: "web", Address: netip.MustParseAddr("10.248.7.2"), PublicIPv4: "203.0.113.5"}
	require.NoError(t, b.Ensure(ctx, v, nil))
	_, err := b.Attach(ctx, v, m)
	require.NoError(t, err)

	kube, _ := clusters.get(ctx, "c1")
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	require.NoError(t, err)
	priv := string(sec.Data[vpcgateway.PrivateKeyKey])
	require.True(t, vpcgateway.ValidKey(priv))
	assert.NotEmpty(t, sec.Data[vpcgateway.StatusTokenKey], "état protégé par un jeton")
	assert.Greater(t, len(ResourceName), 30, "plus long qu'un nom de conteneur du client")

	dep, err := kube.AppsV1().Deployments(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	require.NoError(t, err)
	pod := dep.Spec.Template
	assert.Equal(t, v.ProjectID.String(), pod.Labels["km-project-id"], "membre du projet")
	sc := pod.Spec.Containers[0].SecurityContext
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	assert.Equal(t, []corev1.Capability{"NET_ADMIN"}, sc.Capabilities.Add)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Nil(t, sc.Privileged)

	svc, err := kube.CoreV1().Services(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)
	assert.Equal(t, corev1.ServiceExternalTrafficPolicyLocal, svc.Spec.ExternalTrafficPolicy)

	// Pas encore inscrit : pas de pair, mais l'IP du VPS est admise.
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, cm.Data[vpcgateway.ConfigKey], priv, "aucune clé privée dans la configuration")
	pol, err := kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	require.NoError(t, err)
	raw, _ := json.Marshal(pol.Spec)
	assert.Contains(t, string(raw), "203.0.113.5/32")
	assert.NotContains(t, string(raw), "0.0.0.0/0")

	// Inscription : configuration du VPS, sans secret.
	res, err := b.Enroll(ctx, domain.EnrollRequest{VPC: v, Member: m, PublicKey: vpsKey})
	require.NoError(t, err)
	pub, _ := vpcgateway.PublicKey(priv)
	assert.Equal(t, map[string]string{
		"endpoint": "198.51.100.10:31820", "peer_public_key": pub,
		"allowed_ips": "10.248.7.0/24,10.42.0.0/16,10.43.0.0/16", "keepalive": "25",
	}, res.Config)
	for _, val := range res.Config {
		assert.NotEqual(t, priv, val)
	}
	c, _, err := b.readConfig(ctx, kube, ns)
	require.NoError(t, err)
	require.Len(t, c.Peers, 1)
	assert.Equal(t, vpsKey, c.Peers[0].PublicKey)
	assert.Contains(t, vpcgateway.Render(c, priv), "AllowedIPs = 10.248.7.2/32")

	// Un Ensure sans clé (état en base) ne garde pas une clé retirée.
	require.NoError(t, b.Ensure(ctx, v, []domain.Member{m}))
	c, _, _ = b.readConfig(ctx, kube, ns)
	assert.Empty(t, c.Peers[0].PublicKey)
	// Attach répété : une clé inscrite est gardée.
	m.PublicKey = vpsKey
	require.NoError(t, b.Ensure(ctx, v, []domain.Member{m}))
	m.PublicKey = ""
	_, err = b.Attach(ctx, v, m)
	require.NoError(t, err)
	c, _, _ = b.readConfig(ctx, kube, ns)
	assert.Equal(t, vpsKey, c.Peers[0].PublicKey)

	// Détaché : plus de pair, entrée UDP fermée.
	require.NoError(t, b.Detach(ctx, v, m.VPSID))
	pol, _ = kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	raw, _ = json.Marshal(pol.Spec)
	assert.NotContains(t, string(raw), "203.0.113.5")
	assert.NotContains(t, string(raw), `"protocol":"UDP","port":51820`)

	require.NoError(t, b.Teardown(ctx, v))
	_, err = kube.CoreV1().Secrets(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	assert.Error(t, err, "clé retirée avec la passerelle")
}

func TestEnrollRefusals(t *testing.T) {
	ctx := context.Background()
	b := newBackend(&fakeClusters{}, nil)
	v := vpcAt("10.248.8.0/24")
	m := domain.Member{VPSID: uuid.New(), Name: "x", Address: netip.MustParseAddr("10.248.8.2")}
	_, err := b.Enroll(ctx, domain.EnrollRequest{VPC: v, Member: m, PublicKey: "AAAA"})
	assert.ErrorIs(t, err, domain.ErrInvalidPublicKey)
	_, err = b.Enroll(ctx, domain.EnrollRequest{VPC: v, Member: m, PublicKey: vpsKey})
	assert.ErrorIs(t, err, domain.ErrNotAttached, "pas de passerelle")
	require.NoError(t, b.Ensure(ctx, v, nil))
	_, err = b.Enroll(ctx, domain.EnrollRequest{VPC: v, Member: m, PublicKey: vpsKey})
	assert.ErrorIs(t, err, domain.ErrNotAttached, "VPS inconnu de la passerelle")
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	other := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	b := newBackend(&fakeClusters{}, func(context.Context, kubernetes.Interface, string) ([]vpcgateway.PeerStatus, error) {
		return []vpcgateway.PeerStatus{
			{PublicKey: vpsKey, LatestHandshake: now.Add(-time.Minute).Unix(), RxBytes: 300, TxBytes: 4000},
			{PublicKey: other, LatestHandshake: now.Add(-time.Hour).Unix()},
		}, nil
	})
	b.Now = func() time.Time { return now }
	v := vpcAt("10.248.9.0/24")
	fresh := domain.Member{VPSID: uuid.New(), Name: "a", Address: netip.MustParseAddr("10.248.9.2"), PublicKey: vpsKey}
	stale := domain.Member{VPSID: uuid.New(), Name: "b", Address: netip.MustParseAddr("10.248.9.3"), PublicKey: other}
	pending := domain.Member{VPSID: uuid.New(), Name: "c", Address: netip.MustParseAddr("10.248.9.4")}
	require.NoError(t, b.Ensure(ctx, v, []domain.Member{fresh, stale, pending}))
	list, err := b.Status(ctx, v)
	require.NoError(t, err)
	got := map[uuid.UUID]domain.MemberStatus{}
	for _, s := range list {
		got[s.VPSID] = s
	}
	assert.True(t, got[fresh.VPSID].Connected)
	assert.Equal(t, &domain.Traffic{FromVPS: 300, ToVPS: 4000}, got[fresh.VPSID].Traffic, "reçu par la passerelle = envoyé par le VPS")
	assert.Nil(t, got[pending.VPSID].Traffic, "non inscrit : non mesuré")
	assert.False(t, got[stale.VPSID].Connected)
	assert.NotNil(t, got[stale.VPSID].LastContact)
	assert.False(t, got[pending.VPSID].Connected)
	assert.Nil(t, got[pending.VPSID].LastContact)
}

func TestHostPort(t *testing.T) {
	assert.Equal(t, "edge.example:31000", hostPort("edge.example", 31000))
	assert.Equal(t, "[2001:db8::1]:31000", hostPort("2001:db8::1", 31000))
	assert.True(t, strings.HasPrefix(hostPort("198.51.100.1", 1), "198.51.100.1:"))
}
