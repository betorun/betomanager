package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// Import d'un dump : un Job dans l'espace du projet. Un conteneur d'amorce
// télécharge le dump (URL signée de l'API), puis pg_restore le charge avec
// l'utilisateur « app » de la base (secret <cluster>-app de CloudNativePG).
// Le Job ne joint que le DNS, la base visée et l'API : une politique réseau
// propre aux imports, sans quoi un pod sans politique irait partout.

const (
	importRoleLabel   = "betorun.dev/role"
	importRole        = "db-import"
	importIDLabel     = "betorun.dev/import-id"
	importPolicyName  = "betorun-db-import"
	importDumpDir     = "/dump"
	importDumpFile    = importDumpDir + "/dump.pgc"
	importLogTail     = int64(20)
	importDeadlineSec = int64(3600)
	importTTLSeconds  = int32(86400)
)

func importJobName(id string) string { return "db-import-" + id }

// importJob : le Job d'un import.
func importJob(ns string, s service.ImportJobSpec) *batchv1.Job {
	lbl := map[string]string{importRoleLabel: importRole, importIDLabel: s.ImportID, "app.kubernetes.io/managed-by": "betorun"}
	appSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s.Cluster + "-app"}, Key: key}}
	}
	args := []string{"--no-owner", "--no-acl", "--exit-on-error", "--verbose", "-d", "app"}
	if s.Clean {
		args = append(args, "--clean", "--if-exists")
	}
	args = append(args, importDumpFile)
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	secure := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		RunAsNonRoot:             ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	mount := []corev1.VolumeMount{{Name: "dump", MountPath: importDumpDir}}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: importJobName(s.ImportID), Namespace: ns, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(importDeadlineSec),
			TTLSecondsAfterFinished: ptr(importTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: ptr(int64(26)), RunAsGroup: ptr(int64(26)), FSGroup: ptr(int64(26))},
					InitContainers: []corev1.Container{{
						Name: "fetch", Image: s.FetchImage,
						Command:         []string{"curl", "-fsS", "--retry", "3", "-o", importDumpFile, "--max-filesize", "4294967296"},
						Args:            []string{s.DumpURL},
						Resources:       res,
						SecurityContext: secure,
						VolumeMounts:    mount,
					}},
					Containers: []corev1.Container{{
						Name: "restore", Image: s.Image,
						Command: []string{"pg_restore"},
						Args:    args,
						Env: []corev1.EnvVar{
							{Name: "PGHOST", Value: s.Cluster + "-rw"},
							{Name: "PGPORT", Value: "5432"},
							{Name: "PGUSER", ValueFrom: appSecret("username")},
							{Name: "PGPASSWORD", ValueFrom: appSecret("password")},
							{Name: "PGSSLMODE", Value: "require"},
						},
						Resources:       res,
						SecurityContext: secure,
						VolumeMounts:    mount,
					}},
					Volumes: []corev1.Volume{{Name: "dump", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: ptr(resource.MustParse("5Gi"))}}}},
				},
			},
		},
	}
}

// importPolicy : DNS, bases du projet (5432) et API (adresses privées
// autorisées) ; aucune entrée.
func importPolicy(ns string, egressAllow []string) *networkingv1.NetworkPolicy {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns, pg := intstr.FromInt32(53), intstr.FromInt32(5432)
	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "cnpg.io/cluster", Operator: metav1.LabelSelectorOpExists}}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pg}},
		},
		// API publique (HTTPS) : adresses non privées.
		{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0",
			Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}}}}},
	}
	for _, cidr := range egressAllow {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: importPolicyName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "betorun"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{importRoleLabel: importRole}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// ensureImportPolicy : politique réseau des Jobs de base (import, lecture,
// remise à zéro), créée ou mise à jour.
func (c *CNPG) ensureImportPolicy(ctx context.Context, ns string, egressAllow []string) error {
	pol := importPolicy(ns, egressAllow)
	if _, err := c.Kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, pol, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if _, err := c.Kube.NetworkingV1().NetworkPolicies(ns).Update(ctx, pol, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("import network policy: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("import network policy: %w", err)
	}
	return nil
}

func (c *CNPG) StartImport(ctx context.Context, ns string, spec service.ImportJobSpec) error {
	if err := c.ensureImportPolicy(ctx, ns, spec.EgressAllow); err != nil {
		return err
	}
	if _, err := c.Kube.BatchV1().Jobs(ns).Create(ctx, importJob(ns, spec), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("import job: %w", err)
	}
	return nil
}

func (c *CNPG) ImportState(ctx context.Context, ns, importID string) (service.ImportJobState, error) {
	job, err := c.Kube.BatchV1().Jobs(ns).Get(ctx, importJobName(importID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return service.ImportJobState{}, nil
	}
	if err != nil {
		return service.ImportJobState{}, err
	}
	st := service.ImportJobState{Exists: true}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			st.Done = true
		case batchv1.JobFailed:
			st.Done, st.Failed = true, true
			st.Log = c.importLog(ctx, ns, importID, cond.Message)
		}
	}
	return st, nil
}

// importLog : fin du journal du pod (pg_restore, ou curl si le téléchargement
// a échoué) ; à défaut, le message du Job.
func (c *CNPG) importLog(ctx context.Context, ns, importID, fallback string) string {
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: importIDLabel + "=" + importID})
	if err != nil || len(pods.Items) == 0 {
		return fallback
	}
	pod := pods.Items[0]
	container := "restore"
	for _, s := range pod.Status.InitContainerStatuses {
		if t := s.State.Terminated; t != nil && t.ExitCode != 0 {
			container = s.Name
		}
	}
	tail := importLogTail
	stream, err := c.Kube.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return fallback
	}
	defer stream.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, io.LimitReader(stream, 8<<10))
	if out := strings.TrimSpace(b.String()); out != "" {
		return out
	}
	return fallback
}

func ptr[T any](v T) *T { return &v }
