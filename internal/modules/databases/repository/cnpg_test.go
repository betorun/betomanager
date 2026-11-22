package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// Job de remise à zéro : copie puis envoi en amorces, la base n'est vidée
// qu'après ; connecté en « app » (secret du Cluster), politique des imports.
func TestResetJobSpec(t *testing.T) {
	tg := service.TaskTarget{Cluster: "db-main", Image: "pg:17"}
	job := taskJob("km-p", "db-reset-1", "1", tg, []corev1.Container{
		{Name: "dump", Image: "pg:17"}, {Name: "upload", Image: "curl:1"},
	}, corev1.Container{Name: "main", Image: "pg:17"}, resetTimeout)
	spec := job.Spec.Template.Spec
	require.Len(t, spec.InitContainers, 2)
	assert.Equal(t, "dump", spec.InitContainers[0].Name)
	assert.Equal(t, "upload", spec.InitContainers[1].Name)
	assert.Empty(t, spec.InitContainers[1].Env, "l'envoi ne reçoit pas le mot de passe")
	assert.Equal(t, importRole, job.Spec.Template.Labels[importRoleLabel])
	var pw *corev1.EnvVar
	for i, e := range spec.Containers[0].Env {
		if e.Name == "PGPASSWORD" {
			pw = &spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, pw)
	assert.Equal(t, "db-main-app", pw.ValueFrom.SecretKeyRef.Name)
	assert.False(t, *spec.AutomountServiceAccountToken)
}

// Forme exacte de ce qui est créé sur le cluster.
func TestCNPGObjects(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	kube := kubefake.NewClientset()
	c := &CNPG{Dynamic: dyn, Kube: kube}
	spec := service.ClusterSpec{Name: "db-main", DatabaseID: "d1", Storage: "2Gi"}
	if err := c.Create(ctx, "km-demo", spec); err != nil {
		t.Fatal(err)
	}
	obj, err := dyn.Resource(ClusterGVR).Namespace("km-demo").Get(ctx, "db-main", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	size, _, _ := unstructured.NestedString(obj.Object, "spec", "storage", "size")
	limit, _, _ := unstructured.NestedString(obj.Object, "spec", "resources", "limits", "memory")
	super, _, _ := unstructured.NestedBool(obj.Object, "spec", "enableSuperuserAccess")
	db, _, _ := unstructured.NestedString(obj.Object, "spec", "bootstrap", "initdb", "database")
	if size != "2Gi" || limit == "" || super || db != "app" {
		t.Fatalf("cluster : size=%q limit=%q superuser=%v db=%q", size, limit, super, db)
	}
	np, err := kube.NetworkingV1().NetworkPolicies("km-demo").Get(ctx, "db-main", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if np.Spec.PodSelector.MatchLabels["cnpg.io/cluster"] != "db-main" || len(np.Spec.Ingress) != 2 ||
		np.Spec.Ingress[0].Ports[0].Port.IntValue() != 5432 {
		t.Fatalf("politique réseau : %+v", np.Spec)
	}
	// Création rejouée : sans erreur (idempotente).
	if err := c.Create(ctx, "km-demo", spec); err != nil {
		t.Fatalf("création rejouée : %v", err)
	}
	// Arrêt puis relance : annotation d'hibernation de CloudNativePG.
	if err := c.Hibernate(ctx, "km-demo", "db-main", true); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.State(ctx, "km-demo", "db-main"); !st.Hibernated {
		t.Fatal("hibernation attendue")
	}
	if err := c.Hibernate(ctx, "km-demo", "db-main", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.State(ctx, "km-demo", "db-main"); st.Hibernated {
		t.Fatal("relance attendue")
	}
	if err := c.Hibernate(ctx, "km-demo", "absent", true); !errors.Is(err, errClusterMissing) {
		t.Fatalf("cluster absent : %v", err)
	}
	if err := c.Delete(ctx, "km-demo", "db-main"); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.State(ctx, "km-demo", "db-main"); st.Exists {
		t.Fatal("cluster encore présent")
	}
}

func TestCNPGWithBackups(t *testing.T) {
	ctx := context.Background()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	kube := kubefake.NewClientset()
	c := &CNPG{Dynamic: dyn, Kube: kube, Backup: &service.BackupTarget{Endpoint: "https://s3.example", Bucket: "db-backups",
		Region: "eu", AccessKey: "AK", SecretKey: "SK", Retention: "7d", Schedule: "0 0 3 * * *"}}
	spec := service.ClusterSpec{Name: "db-main", DatabaseID: "d1", ProjectID: "p1", Storage: "1Gi"}
	if err := c.Create(ctx, "km-demo", spec); err != nil {
		t.Fatal(err)
	}
	store, err := dyn.Resource(ObjectStoreGVR).Namespace("km-demo").Get(ctx, "db-main-store", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dest, _, _ := unstructured.NestedString(store.Object, "spec", "configuration", "destinationPath")
	key, _, _ := unstructured.NestedString(store.Object, "spec", "configuration", "s3Credentials", "accessKeyId", "name")
	sideLimit, _, _ := unstructured.NestedString(store.Object, "spec", "instanceSidecarConfiguration", "resources", "limits", "memory")
	if dest != "s3://db-backups/p1/d1" || key != "db-main-backup-s3" || sideLimit == "" {
		t.Fatalf("object store : dest=%q key=%q sidecar=%q", dest, key, sideLimit)
	}
	sec, err := kube.CoreV1().Secrets("km-demo").Get(ctx, "db-main-backup-s3", metav1.GetOptions{})
	if err != nil || sec.StringData["SECRET_ACCESS_KEY"] != "SK" {
		t.Fatalf("secret : %v", err)
	}
	cl, _ := dyn.Resource(ClusterGVR).Namespace("km-demo").Get(ctx, "db-main", metav1.GetOptions{})
	plugins, _, _ := unstructured.NestedSlice(cl.Object, "spec", "plugins")
	if len(plugins) != 1 || plugins[0].(map[string]any)["isWALArchiver"] != true {
		t.Fatalf("plugin d'archivage : %v", plugins)
	}
	sched, err := dyn.Resource(ScheduledBackupGVR).Namespace("km-demo").Get(ctx, "db-main-daily", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	method, _, _ := unstructured.NestedString(sched.Object, "spec", "method")
	immediate, _, _ := unstructured.NestedBool(sched.Object, "spec", "immediate")
	if method != "plugin" || !immediate {
		t.Fatalf("sauvegarde planifiée : method=%q immediate=%v", method, immediate)
	}

	// Dernière sauvegarde lue sur les objets Backup du cluster.
	b := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "b1", "namespace": "km-demo", "labels": map[string]any{"cnpg.io/cluster": "db-main"}},
		"status":   map[string]any{"phase": "completed", "stoppedAt": "2026-11-18T03:00:05Z"},
	}}
	if _, err := dyn.Resource(BackupGVR).Namespace("km-demo").Create(ctx, b, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := c.State(ctx, "km-demo", "db-main")
	if err != nil || !st.BackupsEnabled || st.LastBackup == nil || st.LastBackup.Phase != "completed" || st.LastBackup.StoppedAt.IsZero() {
		t.Fatalf("état des sauvegardes : %+v %v", st, err)
	}

	if err := c.Delete(ctx, "km-demo", "db-main"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(ObjectStoreGVR).Namespace("km-demo").Get(ctx, "db-main-store", metav1.GetOptions{}); err == nil {
		t.Fatal("ObjectStore non supprimé")
	}
	if _, err := kube.CoreV1().Secrets("km-demo").Get(ctx, "db-main-backup-s3", metav1.GetOptions{}); err == nil {
		t.Fatal("clés S3 non supprimées")
	}
}

func TestBackupTargetFrom(t *testing.T) {
	if tg, err := BackupTargetFrom(configs.DatabaseBackupConfig{}); tg != nil || err != nil {
		t.Fatal("sans configuration : désactivé, sans erreur")
	}
	ok := configs.DatabaseBackupConfig{Endpoint: "https://s3.example", Bucket: "db-backups", AccessKey: "a", SecretKey: "s"}
	tg, err := BackupTargetFrom(ok)
	if err != nil || tg.Retention != "7d" || tg.Schedule != "0 0 3 * * *" {
		t.Fatalf("défauts : %+v %v", tg, err)
	}
	for name, mut := range map[string]func(*configs.DatabaseBackupConfig){
		"endpoint sans schéma": func(c *configs.DatabaseBackupConfig) { c.Endpoint = "s3.example" },
		"bucket invalide":      func(c *configs.DatabaseBackupConfig) { c.Bucket = "A" },
		"sans clé":             func(c *configs.DatabaseBackupConfig) { c.SecretKey = "" },
		"rétention":            func(c *configs.DatabaseBackupConfig) { c.Retention = "7 jours" },
		"cron à 5 champs":      func(c *configs.DatabaseBackupConfig) { c.Schedule = "0 3 * * *" },
	} {
		c := ok
		mut(&c)
		if _, err := BackupTargetFrom(c); err == nil {
			t.Errorf("%s : erreur attendue", name)
		}
	}
}

func TestCNPGRestoreObjects(t *testing.T) {
	ctx := context.Background()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	c := &CNPG{Dynamic: dyn, Kube: kubefake.NewClientset()}
	at := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	spec := service.ClusterSpec{Name: "db-copy", DatabaseID: "d2", ProjectID: "p1", Storage: "1Gi",
		Restore: &service.RestoreSource{Cluster: "db-main", TargetTime: &at}}
	if err := c.Create(ctx, "km-demo", spec); !errors.Is(err, domain.ErrBackupsDisabled) {
		t.Fatalf("restauration sans sauvegardes configurées : %v", err)
	}
	c.Backup = &service.BackupTarget{Endpoint: "https://s3.example", Bucket: "db-backups", AccessKey: "a", SecretKey: "s", Retention: "7d", Schedule: "0 0 3 * * *"}
	if err := c.Create(ctx, "km-demo", spec); err != nil {
		t.Fatal(err)
	}
	obj, _ := dyn.Resource(ClusterGVR).Namespace("km-demo").Get(ctx, "db-copy", metav1.GetOptions{})
	source, _, _ := unstructured.NestedString(obj.Object, "spec", "bootstrap", "recovery", "source")
	target, _, _ := unstructured.NestedString(obj.Object, "spec", "bootstrap", "recovery", "recoveryTarget", "targetTime")
	_, hasInitdb, _ := unstructured.NestedMap(obj.Object, "spec", "bootstrap", "initdb")
	ext, _, _ := unstructured.NestedSlice(obj.Object, "spec", "externalClusters")
	plugins, _, _ := unstructured.NestedSlice(obj.Object, "spec", "plugins")
	if source != "origin" || target != "2026-11-18T10:30:00Z" || hasInitdb {
		t.Fatalf("bootstrap : source=%q target=%q initdb=%v", source, target, hasInitdb)
	}
	params := ext[0].(map[string]any)["plugin"].(map[string]any)["parameters"].(map[string]any)
	if params["barmanObjectName"] != "db-main-store" || params["serverName"] != "db-main" {
		t.Fatalf("source des sauvegardes : %v", params)
	}
	own := plugins[0].(map[string]any)["parameters"].(map[string]any)["barmanObjectName"]
	if own != "db-copy-store" {
		t.Fatalf("la copie doit archiver dans son propre dépôt, pas dans celui de la source : %v", own)
	}
}

func TestImportJob_SafeDefaults(t *testing.T) {
	j := importJob("km-demo", service.ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg:17", FetchImage: "curl:1", DumpURL: "http://x/d", Clean: true})
	pod := j.Spec.Template.Spec
	restore := pod.Containers[0]
	if *j.Spec.BackoffLimit != 0 || *pod.AutomountServiceAccountToken || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("Job : une seule tentative, sans jeton Kubernetes")
	}
	args := strings.Join(restore.Args, " ")
	for _, want := range []string{"--no-owner", "--no-acl", "--exit-on-error", "-d app", "--clean --if-exists"} {
		if !strings.Contains(args, want) {
			t.Fatalf("pg_restore %q sans %s", args, want)
		}
	}
	for _, e := range restore.Env {
		if e.Name == "PGPASSWORD" && (e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "db-main-app") {
			t.Fatal("mot de passe : depuis le secret de l'utilisateur app, jamais en clair")
		}
	}
	if *restore.SecurityContext.AllowPrivilegeEscalation || !*restore.SecurityContext.RunAsNonRoot {
		t.Fatal("conteneur sans privilèges")
	}
	pol := importPolicy("km-demo", []string{"10.42.0.1/32"})
	if pol.Spec.PodSelector.MatchLabels[importRoleLabel] != importRole || len(pol.Spec.Ingress) != 0 || len(pol.Spec.Egress) != 4 {
		t.Fatalf("politique : %+v", pol.Spec)
	}
}

func TestImportState_FromJob(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewSimpleClientset()
	c := &CNPG{Kube: kube}
	if st, _ := c.ImportState(ctx, "km-demo", "i1"); st.Exists {
		t.Fatal("Job absent")
	}
	if err := c.StartImport(ctx, "km-demo", service.ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg", FetchImage: "curl", DumpURL: "u"}); err != nil {
		t.Fatal(err)
	}
	// Deuxième départ (reprise) : sans erreur.
	if err := c.StartImport(ctx, "km-demo", service.ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg", FetchImage: "curl", DumpURL: "u"}); err != nil {
		t.Fatal(err)
	}
	job, _ := kube.BatchV1().Jobs("km-demo").Get(ctx, importJobName("i1"), metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}}
	_, _ = kube.BatchV1().Jobs("km-demo").UpdateStatus(ctx, job, metav1.UpdateOptions{})
	st, err := c.ImportState(ctx, "km-demo", "i1")
	if err != nil || !st.Done || !st.Failed || st.Log != "BackoffLimitExceeded" {
		t.Fatalf("état : %+v %v", st, err)
	}
}

var listKinds = map[schema.GroupVersionResource]string{
	ClusterGVR: "ClusterList", BackupGVR: "BackupList", ScheduledBackupGVR: "ScheduledBackupList", ObjectStoreGVR: "ObjectStoreList",
}
