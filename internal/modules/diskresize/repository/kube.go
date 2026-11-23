package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
)

const (
	roleLabel = "betorun.app/role"
	role      = "disk-resize"
	// originAnnotation : ce que le disque temporaire garde du disque d'origine
	// (étiquettes, annotations Helm, classe de stockage), en JSON.
	originAnnotation = "betorun.app/resize-origin"
)

// copyScript : vide la cible, copie, puis compare le nombre d'entrées et la
// somme des tailles. Non-root : un fichier illisible fait échouer la copie.
const copyScript = `set -eo pipefail
find /to -mindepth 1 -delete
cd /from
tar cf - . | tar xf - -C /to
count() { find . | wc -l; }
bytes() { find . -type f -exec stat -c %s {} + | awk '{s+=$1} END {print s+0}'; }
a=$(count); sa=$(bytes)
cd /to
b=$(count); sb=$(bytes)
if [ "$a" != "$b" ] || [ "$sa" != "$sb" ]; then echo "copy mismatch: $a/$b entries, $sa/$sb bytes"; exit 1; fi
echo "copied $a entries, $sa bytes"
`

// KubeClaims : Claims sur un vrai cluster.
type KubeClaims struct {
	// Kube : client du cluster du namespace de chaque projet.
	Kube    k8sprovider.NamespaceClient
	Image   string        // busybox figé
	Timeout time.Duration // par copie (démarrage compris)
	Poll    time.Duration
}

// client : client du cluster du namespace.
func (k *KubeClaims) client(ctx context.Context, ns string) (kubernetes.Interface, error) {
	kube, err := k.Kube(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("project cluster: %w", err)
	}
	return kube, nil
}

func (k *KubeClaims) poll() time.Duration {
	if k.Poll > 0 {
		return k.Poll
	}
	return 2 * time.Second
}

func (k *KubeClaims) timeout() time.Duration {
	if k.Timeout > 0 {
		return k.Timeout
	}
	return time.Hour
}

// wait : rappelle `done` jusqu'à vrai, une erreur ou l'échéance.
func (k *KubeClaims) wait(ctx context.Context, limit time.Duration, what string, done func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		ok, err := done(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: timed out", what)
		case <-time.After(k.poll()):
		}
	}
}

func usesClaim(p corev1.Pod, claim string) bool {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

func (k *KubeClaims) WaitDetached(ctx context.Context, ns, claim string) error {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return err
	}
	return k.wait(ctx, 5*time.Minute, "the container did not stop", func(ctx context.Context) (bool, error) {
		pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, p := range pods.Items {
			if usesClaim(p, claim) && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				return false, nil
			}
		}
		return true, nil
	})
}

func (k *KubeClaims) Size(ctx context.Context, ns, claim string) (string, error) {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return "", err
	}
	pvc, err := kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String(), nil
}

// origin : étiquettes, annotations Helm et classe du disque d'origine.
type origin struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Class       *string           `json:"class,omitempty"`
}

func claimSpec(size string, class *string) corev1.PersistentVolumeClaimSpec {
	return corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		StorageClassName: class,
		Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
	}
}

// CreateTemp : le disque temporaire garde, en annotations, les étiquettes et
// annotations du disque d'origine (Helm le reconnaîtra après recréation).
func (k *KubeClaims) CreateTemp(ctx context.Context, ns, temp, from, size string) error {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return err
	}
	pvcs := kube.CoreV1().PersistentVolumeClaims(ns)
	orig, err := pvcs.Get(ctx, from, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("container disk: %w", err)
	}
	o := origin{Labels: orig.Labels, Annotations: map[string]string{}, Class: orig.Spec.StorageClassName}
	for key, v := range orig.Annotations {
		if strings.HasPrefix(key, "meta.helm.sh/") {
			o.Annotations[key] = v
		}
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	ann := map[string]string{originAnnotation: string(raw)}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: temp, Namespace: ns, Annotations: ann,
			Labels: map[string]string{roleLabel: role, "app.kubernetes.io/managed-by": "betorun"}},
		Spec: claimSpec(size, orig.Spec.StorageClassName),
	}
	if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("temporary disk: %w", err)
	}
	return nil
}

// Recreate : supprime `claim` puis le recrée à `size` avec ce que `temp` a
// gardé. Refuse si le disque temporaire n'est pas là (il porte les données).
func (k *KubeClaims) Recreate(ctx context.Context, ns, claim, temp, size string) error {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return err
	}
	pvcs := kube.CoreV1().PersistentVolumeClaims(ns)
	tmp, err := pvcs.Get(ctx, temp, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("temporary disk missing, the container disk is kept: %w", err)
	}
	var o origin
	if err := json.Unmarshal([]byte(tmp.Annotations[originAnnotation]), &o); err != nil {
		return fmt.Errorf("temporary disk: origin unreadable, the container disk is kept: %w", err)
	}
	if err := pvcs.Delete(ctx, claim, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("container disk: %w", err)
	}
	if err := k.wait(ctx, 5*time.Minute, "the old disk was not released", func(ctx context.Context) (bool, error) {
		_, err := pvcs.Get(ctx, claim, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: ns, Labels: o.Labels, Annotations: o.Annotations},
		Spec:       claimSpec(size, o.Class),
	}
	if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("new container disk: %w", err)
	}
	return nil
}

func (k *KubeClaims) Delete(ctx context.Context, ns, claim string) error {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return err
	}
	err = kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, claim, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func ptr[T any](v T) *T { return &v }

// copyJob : monte les deux disques et lance copyScript avec l'utilisateur
// des fichiers (jamais root).
func (k *KubeClaims) copyJob(ns, from, to string, uid, gid int64) *batchv1.Job {
	if uid <= 0 {
		uid = 1000
	}
	if gid <= 0 {
		gid = uid
	}
	lbl := map[string]string{roleLabel: role, "app.kubernetes.io/managed-by": "betorun"}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-resize-" + rand.String(8), Namespace: ns, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(k.timeout() / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					EnableServiceLinks:           ptr(false),
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &gid, FSGroup: &gid, RunAsNonRoot: ptr(true)},
					Containers: []corev1.Container{{
						Name: "copy", Image: k.Image, Command: []string{"sh", "-c", copyScript},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), RunAsNonRoot: ptr(true),
							Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "from", MountPath: "/from", ReadOnly: true}, {Name: "to", MountPath: "/to"}},
					}},
					Volumes: []corev1.Volume{
						{Name: "from", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: from, ReadOnly: true}}},
						{Name: "to", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: to}}},
					},
				},
			},
		},
	}
}

// ErrCopyFailed : la copie n'a pas abouti (rien n'a été supprimé).
var ErrCopyFailed = errors.New("the disk copy failed")

func (k *KubeClaims) Copy(ctx context.Context, ns, from, to string, uid, gid int64) error {
	kube, err := k.client(ctx, ns)
	if err != nil {
		return err
	}
	job := k.copyJob(ns, from, to, uid, gid)
	jobs := kube.BatchV1().Jobs(ns)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("copy task: %w", err)
	}
	defer func() {
		bg := metav1.DeletePropagationBackground
		_ = jobs.Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	}()
	var failed string
	err = k.wait(ctx, k.timeout()+time.Minute, "the disk copy did not finish in time", func(ctx context.Context) (bool, error) {
		got, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil // passager : on réessaie jusqu'à l'échéance
		}
		for _, c := range got.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				return true, nil
			case batchv1.JobFailed:
				failed = c.Message
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if failed != "" {
		return fmt.Errorf("%w: %s", ErrCopyFailed, lastLog(ctx, kube, ns, job.Name, failed))
	}
	return nil
}

// lastLog : dernière ligne du journal de la copie.
func lastLog(ctx context.Context, kube kubernetes.Interface, ns, job, fallback string) string {
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return fallback
	}
	tail := int64(5)
	stream, err := kube.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &tail}).Stream(ctx)
	if err != nil {
		return fallback
	}
	defer stream.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, io.LimitReader(stream, 8<<10))
	// La dernière ligne utile (tar finit par « error exit delayed… »).
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.Contains(l, "error exit delayed") {
			return l
		}
	}
	return fallback
}
