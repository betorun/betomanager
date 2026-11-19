package diskusage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

// Disk : un disque d'un projet et ce qu'il porte.
type Disk struct {
	Namespace string
	PVC       string
	Kind      string // KindWorkload ou KindDatabase
	// Name : nom du conteneur (release Helm) ou de la base.
	Name          string
	CapacityBytes int64
}

// Measurer : inventaire et mesure des disques d'un espace.
type Measurer interface {
	Disks(ctx context.Context, namespace string) ([]Disk, error)
	Measure(ctx context.Context, d Disk) (int64, error)
}

// Étiquettes posées par le chart (conteneurs) et par CloudNativePG (bases).
const (
	labelInstance    = "app.kubernetes.io/instance"
	labelCNPGCluster = "cnpg.io/cluster"
	labelCNPGRole    = "cnpg.io/pvcRole"
	labelRole        = "betorun.app/role"
	roleDiskUsage    = "disk-usage"
	// cnpgPrefix : un Cluster CloudNativePG s'appelle db-<nom de la base>.
	cnpgPrefix = "db-"
	// Utilisateurs par défaut si aucun pod n'utilise le disque : chart
	// (values.yaml : 1000, fsGroup 2000) et Postgres (26).
	defaultWorkloadUID, defaultWorkloadGroup = int64(1000), int64(2000)
	postgresUID                              = int64(26)
	// nobody : jamais root (les espaces des projets l'interdisent).
	nobodyUID = int64(65534)
)

// KubeMeasurer : un Job par disque, qui monte le disque en lecture seule et
// lance du -sk. Il tourne avec l'utilisateur du pod qui utilise le disque :
// il lit ce que l'application peut lire, sans être root.
type KubeMeasurer struct {
	Kube    kubernetes.Interface
	Image   string        // busybox figé
	Timeout time.Duration // démarrage du pod compris
	Poll    time.Duration
}

func (m *KubeMeasurer) Disks(ctx context.Context, ns string) ([]Disk, error) {
	list, err := m.Kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Disk, 0, len(list.Items))
	for _, pvc := range list.Items {
		if pvc.Status.Phase != corev1.ClaimBound {
			continue
		}
		d := Disk{Namespace: ns, PVC: pvc.Name}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			d.CapacityBytes = q.Value()
		}
		switch {
		case pvc.Labels[labelCNPGCluster] != "":
			// Données seulement (le journal WAL, s'il est à part, n'est pas facturé à part).
			if role := pvc.Labels[labelCNPGRole]; role != "" && role != "PG_DATA" {
				continue
			}
			d.Kind, d.Name = KindDatabase, strings.TrimPrefix(pvc.Labels[labelCNPGCluster], cnpgPrefix)
		case pvc.Labels[labelInstance] != "" && pvc.Name == pvc.Labels[labelInstance]+"-pvc":
			d.Kind, d.Name = KindWorkload, pvc.Labels[labelInstance]
		default:
			continue // disque qui n'est pas à nous
		}
		out = append(out, d)
	}
	return out, nil
}

// owner : utilisateur et groupe du pod qui utilise le disque.
func (m *KubeMeasurer) owner(ctx context.Context, d Disk) (uid, group int64) {
	uid, group = defaultWorkloadUID, defaultWorkloadGroup
	if d.Kind == KindDatabase {
		uid, group = postgresUID, postgresUID
	}
	pods, err := m.Kube.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return uid, group
	}
	for _, p := range pods.Items {
		if p.Labels[labelRole] == roleDiskUsage || !usesClaim(p, d.PVC) {
			continue
		}
		if sc := p.Spec.SecurityContext; sc != nil {
			if sc.RunAsUser != nil {
				uid = *sc.RunAsUser
			}
			if sc.FSGroup != nil {
				group = *sc.FSGroup
			}
		}
		for _, c := range p.Spec.Containers {
			if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
				uid = *c.SecurityContext.RunAsUser
				break
			}
		}
		break
	}
	if uid <= 0 {
		uid = nobodyUID
	}
	return uid, group
}

func usesClaim(p corev1.Pod, claim string) bool {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// jobName : court, stable par disque (un seul Job à la fois), unique par passage.
func jobName(pvc string) string {
	sum := sha256.Sum256([]byte(pvc))
	return "disk-usage-" + hex.EncodeToString(sum[:])[:10] + "-" + rand.String(5)
}

// measureJob : le Job de mesure (exporté pour les tests).
func measureJob(d Disk, image string, uid, group int64, deadline time.Duration) *batchv1.Job {
	lbl := map[string]string{labelRole: roleDiskUsage}
	no, yes := false, true
	deadlineSec := int64(deadline.Seconds())
	ttl := int32(300)
	backoff := int32(0)
	small := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName(d.PVC), Namespace: d.Namespace, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadlineSec, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &no,
					EnableServiceLinks:           &no,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &group,
						// Pas de fsGroup : il changerait les droits des fichiers du client.
						SupplementalGroups: []int64{group},
						SeccompProfile:     &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "du",
						Image: image,
						// Total seul ; les fichiers illisibles sont ignorés (2>/dev/null).
						Command:   []string{"sh", "-c", "du -sk /mnt 2>/dev/null | tail -n 1 | cut -f1"},
						Resources: small,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/mnt", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: d.PVC, ReadOnly: true},
					}}},
				},
			},
		},
	}
}

func (m *KubeMeasurer) Measure(ctx context.Context, d Disk) (int64, error) {
	uid, group := m.owner(ctx, d)
	job := measureJob(d, m.Image, uid, group, m.Timeout)
	jobs := m.Kube.BatchV1().Jobs(d.Namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return 0, err
	}
	bg := metav1.DeletePropagationBackground
	defer func() {
		_ = jobs.Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	}()
	deadline := time.Now().Add(m.Timeout + 30*time.Second)
	for {
		j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if j.Status.Succeeded > 0 {
			return m.result(ctx, d.Namespace, job.Name)
		}
		if j.Status.Failed > 0 {
			return 0, fmt.Errorf("disk usage job failed for %s/%s", d.Namespace, d.PVC)
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("disk usage job timed out for %s/%s", d.Namespace, d.PVC)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(m.Poll):
		}
	}
}

// result : la sortie de du (Kio) en octets.
func (m *KubeMeasurer) result(ctx context.Context, ns, job string) (int64, error) {
	pods, err := m.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return 0, errors.New("disk usage pod not found")
	}
	stream, err := m.Kube.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "du"}).Stream(ctx)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	raw, err := io.ReadAll(io.LimitReader(stream, 256))
	if err != nil {
		return 0, err
	}
	return parseKiB(string(raw))
}

func parseKiB(out string) (int64, error) {
	kib, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("unexpected disk usage output %q", strings.TrimSpace(out))
	}
	return kib * 1024, nil
}
