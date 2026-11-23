package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

func pvc(name string, labels map[string]string, size string, bound bool) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
	if bound {
		p.Status.Phase = corev1.ClaimBound
	}
	return p
}

func TestDisks_Classification(t *testing.T) {
	kube := fake.NewSimpleClientset(
		pvc("api-pvc", map[string]string{labelInstance: "api"}, "1Gi", true),
		pvc("db-shop-1", map[string]string{labelCNPGCluster: "db-shop", labelCNPGRole: "PG_DATA"}, "5Gi", true),
		pvc("db-shop-1-wal", map[string]string{labelCNPGCluster: "db-shop", labelCNPGRole: "PG_WAL"}, "1Gi", true),
		pvc("pending-pvc", map[string]string{labelInstance: "pending"}, "1Gi", false),
		pvc("other", map[string]string{labelInstance: "api"}, "1Gi", true),
		pvc("foreign", nil, "1Gi", true),
	)
	m := &KubeMeasurer{Kube: k8sprovider.Fixed(kube)}
	disks, err := m.Disks(context.Background(), "ns")
	require.NoError(t, err)
	got := map[string]domain.Disk{}
	for _, d := range disks {
		got[d.PVC] = d
	}
	require.Len(t, got, 2)
	assert.Equal(t, domain.Disk{Namespace: "ns", PVC: "api-pvc", Kind: domain.KindWorkload, Name: "api", CapacityBytes: 1 << 30}, got["api-pvc"])
	assert.Equal(t, domain.Disk{Namespace: "ns", PVC: "db-shop-1", Kind: domain.KindDatabase, Name: "shop", CapacityBytes: 5 << 30}, got["db-shop-1"])
}

func TestOwner_FromPodUsingClaim(t *testing.T) {
	uid, grp := int64(999), int64(999)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cache-0", Namespace: "ns"},
		Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, FSGroup: &grp},
			Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "cache-pvc"}}}}}}
	kube := fake.NewSimpleClientset(pod)
	u, g := owner(context.Background(), kube, domain.Disk{Namespace: "ns", PVC: "cache-pvc", Kind: domain.KindWorkload})
	assert.Equal(t, [2]int64{999, 999}, [2]int64{u, g})
	u, g = owner(context.Background(), kube, domain.Disk{Namespace: "ns", PVC: "db-x-1", Kind: domain.KindDatabase})
	assert.Equal(t, [2]int64{26, 26}, [2]int64{u, g}, "aucun pod : utilisateur Postgres")

	root := int64(0)
	pod.Spec.SecurityContext.RunAsUser = &root
	u, _ = owner(context.Background(), fake.NewSimpleClientset(pod), domain.Disk{Namespace: "ns", PVC: "cache-pvc", Kind: domain.KindWorkload})
	assert.Equal(t, nobodyUID, u, "jamais root")
}

func TestMeasureJob_ReadOnlyNonRoot(t *testing.T) {
	j := measureJob(domain.Disk{Namespace: "ns", PVC: "api-pvc"}, "busybox:1.37.0", 1000, 2000, 2*time.Minute)
	assert.True(t, strings.HasPrefix(j.Name, "disk-usage-"))
	assert.LessOrEqual(t, len(j.Name), 63)
	ps := j.Spec.Template.Spec
	assert.True(t, *ps.SecurityContext.RunAsNonRoot)
	assert.Nil(t, ps.SecurityContext.FSGroup, "pas de fsGroup : droits des fichiers inchangés")
	assert.True(t, ps.Volumes[0].PersistentVolumeClaim.ReadOnly)
	c := ps.Containers[0]
	assert.True(t, c.VolumeMounts[0].ReadOnly)
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.Equal(t, int64(120), *j.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int32(0), *j.Spec.BackoffLimit)
}

func TestParseKiB(t *testing.T) {
	n, err := parseKiB("2048\n")
	require.NoError(t, err)
	assert.Equal(t, int64(2<<20), n)
	_, err = parseKiB("du: permission denied")
	assert.Error(t, err)
}

// Lot 2b : les disques d'un namespace sont lus sur le cluster de son projet.
func TestDisks_ReadsProjectCluster(t *testing.T) {
	clusters := map[string]*fake.Clientset{
		"km-a": fake.NewSimpleClientset(),
		"km-b": fake.NewSimpleClientset(func() *corev1.PersistentVolumeClaim {
			p := pvc("api-pvc", map[string]string{labelInstance: "api"}, "1Gi", true)
			p.Namespace = "km-b"
			return p
		}()),
	}
	m := &KubeMeasurer{Kube: func(_ context.Context, ns string) (kubernetes.Interface, error) { return clusters[ns], nil }}
	a, err := m.Disks(context.Background(), "km-a")
	require.NoError(t, err)
	assert.Empty(t, a)
	b, err := m.Disks(context.Background(), "km-b")
	require.NoError(t, err)
	assert.Len(t, b, 1)
}
