package databases

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

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Lecture et remise à zéro d'une base par un Job dans l'espace du projet,
// comme l'import : connecté avec l'utilisateur « app » de la base (secret
// <cluster>-app), jamais en superutilisateur, et sans exec (la plateforme n'a
// pas ce droit, voir deploy/k8s/rbac.yaml). La politique réseau des imports
// s'applique (même étiquette de rôle). L'API attend la fin du Job et lit son
// journal ; le Job est ensuite supprimé.

// Shell : SQL dans une base (CNPG sur un vrai cluster ; nil : indisponible).
type Shell interface {
	Contents(ctx context.Context, ns string, t TaskTarget) (domain.Contents, error)
	// Reset : vide la base. Snapshot non nil : la copie (pg_dump) est d'abord
	// envoyée à l'API ; si l'envoi échoue, rien n'est vidé.
	Reset(ctx context.Context, ns string, t TaskTarget, snapshot *SnapshotUpload) error
}

// TaskTarget : la base visée (Cluster CNPG et son image Postgres).
type TaskTarget struct {
	Cluster string
	Image   string
}

// SnapshotUpload : où le Job envoie la copie (URL signée de l'API).
type SnapshotUpload struct {
	URL         string
	FetchImage  string // image avec curl
	EgressAllow []string
}

const (
	taskIDLabel      = "betorun.dev/task-id"
	taskWorkDir      = "/work"
	taskSnapshotFile = taskWorkDir + "/snapshot.pgc"
	taskLogTail      = int64(20)
	taskPoll         = time.Second
	// Délais : mesure (démarrage du pod compris) et remise à zéro (copie comprise).
	contentsTimeout = 90 * time.Second
	resetTimeout    = 15 * time.Minute
)

// contentsSQL : taille, et tables (ordinaires et partitionnées) par schéma ;
// une seule ligne JSON.
const contentsSQL = `SELECT json_build_object(
  'size_bytes', pg_database_size(current_database()),
  'schemas', COALESCE((
    SELECT json_agg(json_build_object('name', s.name, 'tables', s.tables) ORDER BY s.name)
    FROM (
      SELECT n.nspname AS name, count(c.oid) FILTER (WHERE c.relkind IN ('r', 'p')) AS tables
      FROM pg_namespace n
      LEFT JOIN pg_class c ON c.relnamespace = n.oid
      WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
        AND n.nspname NOT LIKE 'pg\_temp\_%' AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'
      GROUP BY n.nspname
    ) s
  ), '[]'::json))`

// resetSQL : coupe les autres connexions de « app » (les conteneurs
// branchés), puis supprime tout ce qu'il possède dans la base (tables,
// schémas, fonctions, extensions qu'il a créés). La base, l'utilisateur et son
// mot de passe restent : les branchements marchent encore.
const resetSQL = `SET lock_timeout = '15s';
SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
  WHERE datname = current_database() AND usename = current_user AND pid <> pg_backend_pid();
BEGIN;
DROP OWNED BY CURRENT_USER CASCADE;
COMMIT;`

func taskJobName(kind, id string) string { return "db-" + kind + "-" + id }

func newTaskID() string { return uuid.NewString() }

// taskJob : Job d'une tâche (amorces éventuelles, puis le conteneur « main »).
func taskJob(ns, name, taskID string, t TaskTarget, inits []corev1.Container, main corev1.Container, deadline time.Duration) *batchv1.Job {
	lbl := map[string]string{importRoleLabel: importRole, taskIDLabel: taskID, "app.kubernetes.io/managed-by": "betorun"}
	appSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: t.Cluster + "-app"}, Key: key}}
	}
	env := []corev1.EnvVar{
		{Name: "PGHOST", Value: t.Cluster + "-rw"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGDATABASE", Value: domain.DatabaseName},
		{Name: "PGUSER", ValueFrom: appSecret("username")},
		{Name: "PGPASSWORD", ValueFrom: appSecret("password")},
		{Name: "PGSSLMODE", Value: "require"},
		{Name: "PGCONNECT_TIMEOUT", Value: "10"},
	}
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	secure := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		RunAsNonRoot:             ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	mount := []corev1.VolumeMount{{Name: "work", MountPath: taskWorkDir}}
	fill := func(c corev1.Container) corev1.Container {
		if c.Image == t.Image {
			c.Env = env
		}
		c.Resources, c.SecurityContext, c.VolumeMounts = res, secure, mount
		return c
	}
	for i := range inits {
		inits[i] = fill(inits[i])
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(deadline / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: ptr(int64(26)), RunAsGroup: ptr(int64(26)), FSGroup: ptr(int64(26))},
					InitContainers:               inits,
					Containers:                   []corev1.Container{fill(main)},
					Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: ptr(resource.MustParse("5Gi"))}}}},
				},
			},
		},
	}
}

// waitForDB : attend que la base réponde avant la commande. Le pod d'un
// Job neuf voit ses premières connexions refusées tant que la politique
// réseau ne l'a pas pris en compte (une à deux secondes sur k3s).
const waitForDB = `i=0; until pg_isready -q -t 2; do i=$((i+1)); [ "$i" -ge 30 ] && break; sleep 1; done; `

// waitThen : la commande, après l'attente de la base.
func waitThen(cmd ...string) []string {
	return append([]string{"sh", "-c", waitForDB + `exec "$@"`, "sh"}, cmd...)
}

func psqlCommand() []string {
	return []string{"psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1"}
}

func (c *CNPG) Contents(ctx context.Context, ns string, t TaskTarget) (domain.Contents, error) {
	id := newTaskID()
	main := corev1.Container{Name: "main", Image: t.Image, Command: waitThen(append(psqlCommand(), "-c", contentsSQL)...)}
	out, err := c.runTask(ctx, ns, taskJob(ns, taskJobName("contents", id), id, t, nil, main, contentsTimeout), nil, contentsTimeout)
	if err != nil {
		return domain.Contents{}, err
	}
	var v domain.Contents
	if err := json.Unmarshal([]byte(lastLine(out)), &v); err != nil {
		return domain.Contents{}, fmt.Errorf("database contents: %w", err)
	}
	for _, s := range v.Schemas {
		v.Tables += s.Tables
	}
	return v, nil
}

func (c *CNPG) Reset(ctx context.Context, ns string, t TaskTarget, snap *SnapshotUpload) error {
	id := newTaskID()
	var inits []corev1.Container
	var allow []string
	if snap != nil {
		allow = snap.EgressAllow
		inits = []corev1.Container{
			{Name: "dump", Image: t.Image, Command: waitThen("pg_dump", "-Fc", "--no-owner", "--no-acl", "-f", taskSnapshotFile)},
			{Name: "upload", Image: snap.FetchImage, Command: []string{"curl", "-fsS", "--retry", "3", "-X", "PUT",
				"-H", "Content-Type: application/octet-stream", "-T", taskSnapshotFile}, Args: []string{snap.URL}},
		}
	}
	// Plusieurs instructions (BEGIN/COMMIT) : par l'entrée de psql, pas -c.
	main := corev1.Container{Name: "main", Image: t.Image,
		Command: []string{"sh", "-c", waitForDB + `printf '%s\n' "$RESET_SQL" | psql -X -q -At -v ON_ERROR_STOP=1`}}
	job := taskJob(ns, taskJobName("reset", id), id, t, inits, main, resetTimeout)
	mc := &job.Spec.Template.Spec.Containers[0]
	mc.Env = append(append([]corev1.EnvVar{}, mc.Env...), corev1.EnvVar{Name: "RESET_SQL", Value: resetSQL})
	_, err := c.runTask(ctx, ns, job, allow, resetTimeout)
	return err
}

// runTask : crée le Job, attend sa fin, rend le journal du conteneur « main »
// (ou l'erreur du conteneur en échec), puis supprime le Job.
func (c *CNPG) runTask(ctx context.Context, ns string, job *batchv1.Job, egressAllow []string, timeout time.Duration) (string, error) {
	if err := c.ensureImportPolicy(ctx, ns, egressAllow); err != nil {
		return "", err
	}
	jobs := c.Kube.BatchV1().Jobs(ns)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("database task: %w", err)
	}
	cleanup := func() {
		bg := metav1.DeletePropagationBackground
		_ = jobs.Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	}
	defer cleanup()
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	taskID := job.Labels[taskIDLabel]
	for {
		got, err := jobs.Get(wait, job.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			if wait.Err() != nil {
				return "", ErrTaskTimeout
			}
			return "", err
		}
		if got != nil {
			for _, cond := range got.Status.Conditions {
				if cond.Status != corev1.ConditionTrue {
					continue
				}
				switch cond.Type {
				case batchv1.JobComplete:
					return c.taskLog(ctx, ns, taskID, "main"), nil
				case batchv1.JobFailed:
					return "", &TaskError{Log: c.failedLog(ctx, ns, taskID, cond.Message)}
				}
			}
		}
		select {
		case <-wait.Done():
			return "", ErrTaskTimeout
		case <-time.After(taskPoll):
		}
	}
}

// ErrTaskTimeout : la tâche n'a pas fini à temps (pod pas démarré, base lente).
var ErrTaskTimeout = errors.New("the database task did not finish in time")

// TaskError : la tâche a échoué ; Log : fin du journal du conteneur fautif.
type TaskError struct{ Log string }

func (e *TaskError) Error() string { return "the database task failed: " + e.Log }

func (c *CNPG) taskPod(ctx context.Context, ns, taskID string) (*corev1.Pod, error) {
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: taskIDLabel + "=" + taskID})
	if err != nil || len(pods.Items) == 0 {
		return nil, errors.New("no pod")
	}
	return &pods.Items[0], nil
}

func (c *CNPG) taskLog(ctx context.Context, ns, taskID, container string) string {
	pod, err := c.taskPod(ctx, ns, taskID)
	if err != nil {
		return ""
	}
	tail := taskLogTail
	stream, err := c.Kube.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, io.LimitReader(stream, 64<<10))
	return strings.TrimSpace(b.String())
}

// failedLog : journal du premier conteneur en échec (amorce ou main).
func (c *CNPG) failedLog(ctx context.Context, ns, taskID, fallback string) string {
	pod, err := c.taskPod(ctx, ns, taskID)
	if err != nil {
		return fallback
	}
	container := "main"
	for _, s := range pod.Status.InitContainerStatuses {
		if t := s.State.Terminated; t != nil && t.ExitCode != 0 {
			container = s.Name
			break
		}
	}
	if out := c.taskLog(ctx, ns, taskID, container); out != "" {
		return container + ": " + out
	}
	return fallback
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
