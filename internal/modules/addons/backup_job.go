package addons

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Jobs de sauvegarde et de restauration. Le pod porte les étiquettes du
// projet : la politique réseau du service l'accepte comme un conteneur du
// projet. Sa propre politique ne laisse sortir que vers le DNS, les pods du
// projet et l'API (adresses publiques, ou celles de builds.egress_allow).

const (
	backupRoleLabel  = "betorun.dev/role"
	backupRole       = "addon-backup"
	backupTaskLabel  = "betorun.dev/task-id"
	backupPolicyName = "betorun-addon-backup"
	backupWorkDir    = "/work"
	backupFile       = backupWorkDir + "/backup.gz"
	backupLogTail    = int64(20)
)

// KubeBackups : BackupRunner sur un vrai cluster.
type KubeBackups struct {
	Kube kubernetes.Interface
	Poll time.Duration
}

func ptr[T any](v T) *T { return &v }

// backupJob : amorce puis conteneur principal ; l'un des deux est l'image
// du service (commande du modèle), l'autre curl (transfert avec l'API).
func backupJob(t BackupTask, projectID string, restore bool) *batchv1.Job {
	lbl := map[string]string{
		backupRoleLabel: backupRole, backupTaskLabel: t.TaskID,
		"app.kubernetes.io/managed-by": "betorun",
		// Réseau privé du projet (chart standard-app) : le service accepte ce pod.
		"app.kubernetes.io/part-of": "kubemanager", "km-project-id": projectID,
	}
	env := []corev1.EnvVar{
		{Name: "ADDON_HOST", Value: t.Host},
		{Name: "BACKUP_FILE", Value: backupFile},
		{Name: t.Commands.PasswordEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: utils.EnvSecretName(t.Release)}, Key: passwordKey}}},
	}
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	secure := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false), RunAsNonRoot: ptr(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	mount := []corev1.VolumeMount{{Name: "work", MountPath: backupWorkDir}}
	script := t.Commands.Wait + "\n" + t.Commands.Dump
	if restore {
		script = t.Commands.Wait + "\n" + t.Commands.Restore
	}
	service := corev1.Container{Name: "service", Image: t.Image, Command: []string{"bash", "-c", "set -o pipefail\n" + script},
		Env: env, Resources: res, SecurityContext: secure, VolumeMounts: mount}
	var transfer corev1.Container
	if restore {
		transfer = corev1.Container{Name: "fetch", Image: t.FetchImage,
			Command: []string{"curl", "-fsS", "--retry", "3", "-o", backupFile}, Args: []string{t.URL}}
	} else {
		transfer = corev1.Container{Name: "upload", Image: t.FetchImage, Command: []string{"curl", "-fsS", "--retry", "3", "-X", "PUT",
			"-H", "Content-Type: application/gzip", "-T", backupFile}, Args: []string{t.URL}}
	}
	transfer.Resources, transfer.SecurityContext, transfer.VolumeMounts = res, secure, mount
	init, main := service, transfer
	if restore {
		init, main = transfer, service
	}
	kind := "backup"
	if restore {
		kind = "restore"
	}
	uid := t.RunAsUser
	if uid <= 0 {
		uid = 65534
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "addon-" + kind + "-" + t.TaskID, Namespace: t.Namespace, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(backupTimeout / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					EnableServiceLinks:           ptr(false),
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid},
					InitContainers:               []corev1.Container{init},
					Containers:                   []corev1.Container{main},
					Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: ptr(resource.MustParse("20Gi"))}}}},
				},
			},
		},
	}
}

// passwordKey : variable secrète du conteneur porteur (domain.PasswordVariable).
const passwordKey = "ADDON_PASSWORD"

// backupPolicy : sorties permises aux Jobs de sauvegarde.
func backupPolicy(ns string, egressAllow []string) *networkingv1.NetworkPolicy {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(53)
	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
		},
		// Les services du projet (même espace).
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		// API publique : adresses non privées.
		{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0",
			Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}}}}},
	}
	for _, cidr := range egressAllow {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: backupPolicyName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "betorun"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{backupRoleLabel: backupRole}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

func (k *KubeBackups) ensurePolicy(ctx context.Context, ns string, egressAllow []string) error {
	pol := backupPolicy(ns, egressAllow)
	pols := k.Kube.NetworkingV1().NetworkPolicies(ns)
	if _, err := pols.Create(ctx, pol, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if _, err := pols.Update(ctx, pol, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("backup network policy: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("backup network policy: %w", err)
	}
	return nil
}

func (k *KubeBackups) Dump(ctx context.Context, t BackupTask) error    { return k.run(ctx, t, false) }
func (k *KubeBackups) Restore(ctx context.Context, t BackupTask) error { return k.run(ctx, t, true) }

// ErrBackupTimeout : la tâche n'a pas fini à temps.
var ErrBackupTimeout = errors.New("the backup task did not finish in time")

func (k *KubeBackups) run(ctx context.Context, t BackupTask, restore bool) error {
	if err := k.ensurePolicy(ctx, t.Namespace, t.EgressAllow); err != nil {
		return err
	}
	job := backupJob(t, t.ProjectID, restore)
	jobs := k.Kube.BatchV1().Jobs(t.Namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("backup task: %w", err)
	}
	defer func() {
		bg := metav1.DeletePropagationBackground
		_ = jobs.Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	}()
	wait, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	poll := k.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		got, err := jobs.Get(wait, job.Name, metav1.GetOptions{})
		if err != nil && wait.Err() != nil {
			return ErrBackupTimeout
		}
		if err == nil {
			for _, c := range got.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					continue
				}
				switch c.Type {
				case batchv1.JobComplete:
					return nil
				case batchv1.JobFailed:
					return fmt.Errorf("the backup task failed: %s", k.failedLog(ctx, t, c.Message))
				}
			}
		}
		select {
		case <-wait.Done():
			return ErrBackupTimeout
		case <-time.After(poll):
		}
	}
}

// failedLog : fin du journal du premier conteneur en échec.
func (k *KubeBackups) failedLog(ctx context.Context, t BackupTask, fallback string) string {
	pods, err := k.Kube.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: backupTaskLabel + "=" + t.TaskID})
	if err != nil || len(pods.Items) == 0 {
		return fallback
	}
	pod := pods.Items[0]
	container := pod.Spec.Containers[0].Name
	for _, s := range pod.Status.InitContainerStatuses {
		if term := s.State.Terminated; term != nil && term.ExitCode != 0 {
			container = s.Name
			break
		}
	}
	tail := backupLogTail
	stream, err := k.Kube.CoreV1().Pods(t.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return fallback
	}
	defer stream.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, io.LimitReader(stream, 16<<10))
	if out := strings.TrimSpace(b.String()); out != "" {
		return container + ": " + lastLine(out)
	}
	return fallback
}
