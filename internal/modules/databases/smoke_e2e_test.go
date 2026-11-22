package databases

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// TestE2E_CNPGOnRealCluster : le vrai code contre un vrai cluster avec
// CloudNativePG. Lancé à la main, jamais en CI :
//
//	CNPG_E2E_KUBECONFIG=~/sykdev/betorun/kubeconfig.terrain go test ./internal/modules/databases/ -run TestE2E -v -count=1
//
// Crée un espace jetable, une base, attend qu'elle soit prête, lit son URI,
// puis supprime tout.
func TestE2E_CNPGOnRealCluster(t *testing.T) {
	path := os.Getenv("CNPG_E2E_KUBECONFIG")
	if path == "" {
		t.Skip("CNPG_E2E_KUBECONFIG non défini")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	ns := "km-e2e-cnpg-" + time.Now().Format("150405")
	if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})

	c := &repository.CNPG{Dynamic: dyn, Kube: kube}
	spec := service.ClusterSpec{Name: "db-smoke", DatabaseID: "e2e", ProjectID: "e2e", Storage: "1Gi"}
	if err := c.Create(ctx, ns, spec); err != nil {
		t.Fatalf("création : %v", err)
	}
	var st service.ClusterState
	for {
		st, err = c.State(ctx, ns, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("phase : %q prête=%v image=%s", st.Phase, st.ReadyInstance, st.Image)
		if st.ReadyInstance {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pas prête à temps (dernière phase %q)", st.Phase)
		case <-time.After(10 * time.Second):
		}
	}
	uri, err := c.AppURI(ctx, ns, spec.Name)
	if err != nil || !strings.HasPrefix(uri, "postgresql://app:") || !strings.Contains(uri, "db-smoke-rw") {
		t.Fatalf("URI : err=%v préfixe=%q", err, firstChars(uri, 18))
	}
	t.Logf("version : %s", service.ImageVersion(st.Image))
	if err := c.Delete(ctx, ns, spec.Name); err != nil {
		t.Fatalf("suppression : %v", err)
	}
}

func firstChars(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
