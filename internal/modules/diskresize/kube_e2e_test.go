package diskresize

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKubeClaims_E2E : déroule disque temporaire, copie, recréation et
// recopie sur un vrai cluster. Lancé seulement avec DISKRESIZE_E2E_NS (un
// espace jetable, créé puis supprimé) et KUBECONFIG.
func TestKubeClaims_E2E(t *testing.T) {
	ns := os.Getenv("DISKRESIZE_E2E_NS")
	if ns == "" {
		t.Skip("DISKRESIZE_E2E_NS non défini")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	defer func() {
		_ = kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}()
	k := &KubeClaims{Kube: kube, Image: "busybox:1.37.0", Timeout: 5 * time.Minute, Poll: time.Second}
	class := "local-path"
	orig := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "rz-pvc", Namespace: ns,
			Labels:      map[string]string{"app.kubernetes.io/instance": "rz", "app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{"meta.helm.sh/release-name": "rz", "meta.helm.sh/release-namespace": ns}},
		Spec: claimSpec("1Gi", &class),
	}
	if _, err := kube.CoreV1().PersistentVolumeClaims(ns).Create(ctx, orig, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Données de départ, écrites par l'utilisateur de l'application.
	run(t, ctx, k, ns, "rz-pvc", `mkdir -p "/d/a b" && echo hello > "/d/a b/f" && echo x > /d/.hidden && ln -s "a b" /d/link && head -c 3000000 /dev/urandom > /d/big && md5sum /d/big | cut -d" " -f1 > /d/big.md5`)

	if err := k.WaitDetached(ctx, ns, "rz-pvc"); err != nil {
		t.Fatal(err)
	}
	if err := k.CreateTemp(ctx, ns, "rz-pvc-resize", "rz-pvc", "2Gi"); err != nil {
		t.Fatal(err)
	}
	if err := k.Copy(ctx, ns, "rz-pvc", "rz-pvc-resize", 1000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := k.Recreate(ctx, ns, "rz-pvc", "rz-pvc-resize", "2Gi"); err != nil {
		t.Fatal(err)
	}
	if size, _ := k.Size(ctx, ns, "rz-pvc"); size != "2Gi" {
		t.Fatalf("size = %q", size)
	}
	if err := k.Copy(ctx, ns, "rz-pvc-resize", "rz-pvc", 1000, 2000); err != nil {
		t.Fatal(err)
	}
	got, err := kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "rz-pvc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "Helm" || got.Annotations["meta.helm.sh/release-name"] != "rz" || got.Labels["app.kubernetes.io/instance"] != "rz" {
		t.Fatalf("labels %v annotations %v", got.Labels, got.Annotations)
	}
	if err := k.Delete(ctx, ns, "rz-pvc-resize"); err != nil {
		t.Fatal(err)
	}
	// Les données ont fait l'aller-retour.
	run(t, ctx, k, ns, "rz-pvc", `test "$(cat "/d/a b/f")" = hello && test -f /d/.hidden && test -L /d/link && test "$(md5sum /d/big | cut -d" " -f1)" = "$(cat /d/big.md5)"`)

	// Fichier illisible : la copie échoue, la cible n'est pas validée.
	run(t, ctx, k, ns, "rz-pvc", `echo s > /d/secret && chmod 000 /d/secret`)
	if err := k.CreateTemp(ctx, ns, "rz-pvc-resize", "rz-pvc", "5Gi"); err != nil {
		t.Fatal(err)
	}
	err = k.Copy(ctx, ns, "rz-pvc", "rz-pvc-resize", 1000, 2000)
	if err == nil || !strings.Contains(err.Error(), "the disk copy failed") {
		t.Fatalf("unreadable file: err = %v", err)
	}
	t.Logf("échec attendu : %v", err)
}

// run : commande sur le disque monté en /d, avec l'utilisateur 1000.
func run(t *testing.T, ctx context.Context, k *KubeClaims, ns, claim, script string) {
	t.Helper()
	job := k.copyJob(ns, claim, claim, 1000, 2000)
	c := &job.Spec.Template.Spec.Containers[0]
	c.Command = []string{"sh", "-c", "set -e\n" + script}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "to", MountPath: "/d"}}
	job.Spec.Template.Spec.Volumes = job.Spec.Template.Spec.Volumes[1:]
	c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("64Mi")
	jobs := k.Kube.BatchV1().Jobs(ns)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	bg := metav1.DeletePropagationForeground
	defer func() { _ = jobs.Delete(context.Background(), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg}) }()
	err := k.wait(ctx, 5*time.Minute, "job", func(ctx context.Context) (bool, error) {
		got, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, cond := range got.Status.Conditions {
			if cond.Status == corev1.ConditionTrue && cond.Type == batchv1.JobComplete {
				return true, nil
			}
			if cond.Status == corev1.ConditionTrue && cond.Type == batchv1.JobFailed {
				t.Fatalf("job %s failed: %s", job.Name, k.lastLog(ctx, ns, job.Name, cond.Message))
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Le disque doit être libre pour l'étape suivante.
	_ = k.wait(ctx, 2*time.Minute, "job pod", func(ctx context.Context) (bool, error) {
		_ = jobs.Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
		pods, err := k.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job.Name})
		return err == nil && len(pods.Items) == 0, nil
	})
}
