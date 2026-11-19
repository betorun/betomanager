package databases

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/iam"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

type memRepo struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.Database
}

func (m *memRepo) Create(_ context.Context, d *domain.Database) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.CreatedAt = time.Now()
	cp := *d
	m.rows[d.ID] = &cp
	return nil
}
func (m *memRepo) List(_ context.Context, p uuid.UUID) ([]domain.Database, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Database
	for _, d := range m.rows {
		if d.ProjectID == p {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (m *memRepo) Get(_ context.Context, p, id uuid.UUID) (*domain.Database, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.rows[id]
	if !ok || d.ProjectID != p {
		return nil, domain.ErrNotFound
	}
	cp := *d
	return &cp, nil
}
func (m *memRepo) SetStatus(_ context.Context, id uuid.UUID, s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id].Status = s
	return nil
}
func (m *memRepo) Delete(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

type projects struct{ status string }

func (p projects) GetProjectByID(_ context.Context, id string) (*dauth.Project, error) {
	return &dauth.Project{ID: uuid.MustParse(id), Name: "demo", Status: p.status}, nil
}

type fakeClusters struct {
	created  []ClusterSpec
	deleted  []string
	state    ClusterState
	createEr error
}

func (f *fakeClusters) Create(_ context.Context, _ string, s ClusterSpec) error {
	f.created = append(f.created, s)
	return f.createEr
}
func (f *fakeClusters) State(context.Context, string, string) (ClusterState, error) {
	return f.state, nil
}
func (f *fakeClusters) AppURI(context.Context, string, string) (string, error) {
	if !f.state.ReadyInstance {
		return "", errAppSecretMissing
	}
	return "postgresql://app:s3cret@db-main-rw.km-demo:5432/app", nil
}

func (f *fakeClusters) Delete(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func svc(c Clusters) (*Service, *memRepo) {
	r := &memRepo{rows: map[uuid.UUID]*domain.Database{}}
	return &Service{Repo: r, Projects: projects{status: "READY"}, Clusters: c, Logger: zap.NewNop().Sugar()}, r
}

func TestCreateValidatesAndProvisions(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, repo := svc(fc)
	for _, bad := range []domain.CreateDatabaseRequest{{Name: "A"}, {Name: "Bad_Name"}, {Name: "ok-db", StorageSize: "500Gi"}} {
		if _, err := s.Create(ctx, p, bad, "u"); err == nil {
			t.Fatalf("%+v : refus attendu", bad)
		}
	}
	v, err := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != domain.StatusProvisioning || v.Host != "db-main-rw."+utils.ProjectNamespace("demo")+".svc" || v.StorageSize != "1Gi" || v.Port != 5432 {
		t.Fatalf("vue : %+v", v)
	}
	if len(fc.created) != 1 || fc.created[0].Name != "db-main" || fc.created[0].Storage != "1Gi" {
		t.Fatalf("cluster : %+v", fc.created)
	}
	if _, err := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u"); !errors.Is(err, domain.ErrNameTaken) {
		t.Fatalf("nom pris : %v", err)
	}
	// Prête : l'état lu sur le Cluster fait passer la ligne à READY.
	fc.state = ClusterState{Exists: true, ReadyInstance: true, Phase: "Cluster in healthy state", Image: "ghcr.io/cloudnative-pg/postgresql:17.6"}
	list, _ := s.List(ctx, p)
	if list[0].Status != domain.StatusReady || list[0].Version != "17.6" || repo.rows[uuid.MustParse(list[0].ID)].Status != domain.StatusReady {
		t.Fatalf("liste : %+v", list)
	}
}

func TestCreateFailureMarksFailedAndOperatorMissing(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo := svc(&fakeClusters{createEr: ErrOperatorMissing})
	if _, err := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("opérateur absent : %v", err)
	}
	for _, d := range repo.rows {
		if d.Status != domain.StatusFailed {
			t.Fatalf("ligne : %s (FAILED attendu)", d.Status)
		}
	}
	unready, _ := svc(&fakeClusters{})
	unready.Projects = projects{status: "PENDING"}
	if _, err := unready.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u"); err == nil {
		t.Fatal("projet pas prêt : refus attendu")
	}
	off := &Service{Repo: repo}
	if _, err := off.Create(ctx, p, domain.CreateDatabaseRequest{Name: "x-db"}, "u"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("sans cluster : %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, repo := svc(fc)
	v, _ := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u")
	if err := s.Delete(ctx, uuid.New(), uuid.MustParse(v.ID)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("base d'un autre projet : %v", err)
	}
	if err := s.Delete(ctx, p, uuid.MustParse(v.ID)); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "db-main" || len(repo.rows) != 0 {
		t.Fatalf("suppression : %v %d", fc.deleted, len(repo.rows))
	}
}

// Forme exacte de ce qui est créé sur le cluster.
func TestCNPGObjects(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	kube := kubefake.NewClientset()
	c := &CNPG{Dynamic: dyn, Kube: kube}
	spec := ClusterSpec{Name: "db-main", DatabaseID: "d1", Storage: "2Gi"}
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
	if err := c.Delete(ctx, "km-demo", "db-main"); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.State(ctx, "km-demo", "db-main"); st.Exists {
		t.Fatal("cluster encore présent")
	}
}

type fakeWorkloads struct {
	project uuid.UUID
	set     map[string]string
}

func (f *fakeWorkloads) ProjectOf(context.Context, uuid.UUID) (uuid.UUID, error) {
	return f.project, nil
}
func (f *fakeWorkloads) SetSecret(_ context.Context, _ uuid.UUID, name, value string) (bool, error) {
	f.set[name] = value
	return true, nil
}

func TestAttach(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, _ := svc(fc)
	wl := &fakeWorkloads{project: p, set: map[string]string{}}
	s.Workloads = wl
	v, _ := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u")
	id := uuid.MustParse(v.ID)

	if _, err := s.Attach(ctx, p, id, uuid.New(), ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("base pas prête : %v", err)
	}
	fc.state = ClusterState{Exists: true, ReadyInstance: true}
	if _, err := s.Attach(ctx, p, id, uuid.New(), "bad-name"); !errors.Is(err, domain.ErrInvalidVariable) {
		t.Fatalf("variable invalide : %v", err)
	}
	other := &fakeWorkloads{project: uuid.New(), set: map[string]string{}}
	s.Workloads = other
	if _, err := s.Attach(ctx, p, id, uuid.New(), ""); !errors.Is(err, domain.ErrWorkloadNotFound) {
		t.Fatalf("conteneur d'un autre projet : %v", err)
	}
	s.Workloads = wl
	res, err := s.Attach(ctx, p, id, uuid.New(), "")
	if err != nil || res.Variable != "DATABASE_URL" || !res.Redeploy {
		t.Fatalf("attache : %+v %v", res, err)
	}
	if wl.set["DATABASE_URL"] == "" {
		t.Fatal("URI non posée en secret")
	}
	raw := fmt.Sprintf("%+v", res)
	if strings.Contains(raw, "s3cret") {
		t.Fatal("le mot de passe ne doit jamais sortir de l'API")
	}
}

type denyAll struct{ allowed bool }

func (d denyAll) Check(context.Context, iam.Principal, iam.Permission, iam.ResourceName) (iam.Decision, error) {
	return iam.Decision{Allowed: d.allowed}, nil
}

// Brancher une base exige AUSSI le droit sur les variables du conteneur.
func TestAttachChecksWorkloadPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, _ := svc(&fakeClusters{state: ClusterState{Exists: true, ReadyInstance: true}})
	s.Workloads = &fakeWorkloads{set: map[string]string{}}
	h := &Controller{Service: s, IAM: denyAll{allowed: false}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Set("userID", uuid.NewString())
	_, err := h.Attach(c, &domain.AttachRequest{ProjectID: uuid.NewString(), DatabaseID: uuid.NewString(), WorkloadID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "workloads.variables.update") {
		t.Fatalf("refus attendu : %v", err)
	}
}

var listKinds = map[schema.GroupVersionResource]string{
	ClusterGVR: "ClusterList", BackupGVR: "BackupList", ScheduledBackupGVR: "ScheduledBackupList", ObjectStoreGVR: "ObjectStoreList",
}

func TestCNPGWithBackups(t *testing.T) {
	ctx := context.Background()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	kube := kubefake.NewClientset()
	c := &CNPG{Dynamic: dyn, Kube: kube, Backup: &BackupTarget{Endpoint: "https://s3.example", Bucket: "db-backups",
		Region: "eu", AccessKey: "AK", SecretKey: "SK", Retention: "7d", Schedule: "0 0 3 * * *"}}
	spec := ClusterSpec{Name: "db-main", DatabaseID: "d1", ProjectID: "p1", Storage: "1Gi"}
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

func TestRestore(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, repo := svc(fc)
	src, _ := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main", StorageSize: "5Gi"}, "u")
	srcID := uuid.MustParse(src.ID)

	if _, err := s.Restore(ctx, p, srcID, "main-copy", nil, "u"); !errors.Is(err, domain.ErrNoBackup) {
		t.Fatalf("sans sauvegarde : %v", err)
	}
	fc.state = ClusterState{Exists: true, ReadyInstance: true, BackupsEnabled: true,
		LastBackup: &BackupState{Phase: "completed", StoppedAt: time.Now().Add(-time.Hour)}}
	future := time.Now().Add(time.Hour)
	if _, err := s.Restore(ctx, p, srcID, "main-copy", &future, "u"); !errors.Is(err, domain.ErrInvalidTarget) {
		t.Fatalf("instant futur : %v", err)
	}
	if _, err := s.Restore(ctx, p, srcID, "main", nil, "u"); !errors.Is(err, domain.ErrNameTaken) {
		t.Fatalf("nom pris : %v", err)
	}
	at := time.Now().Add(-30 * time.Minute)
	v, err := s.Restore(ctx, p, srcID, "main-copy", &at, "u")
	if err != nil {
		t.Fatal(err)
	}
	last := fc.created[len(fc.created)-1]
	if last.Restore == nil || last.Restore.Cluster != "db-main" || !last.Restore.TargetTime.Equal(at) || last.Storage != "5Gi" {
		t.Fatalf("spec de restauration : %+v", last)
	}
	if v.RestoredFrom != src.ID || repo.rows[uuid.MustParse(v.ID)].RestoredFrom == nil {
		t.Fatalf("origine non enregistrée : %+v", v)
	}
	if repo.rows[srcID].Status == domain.StatusDeleting {
		t.Fatal("la source ne doit jamais être touchée")
	}
}

func TestCNPGRestoreObjects(t *testing.T) {
	ctx := context.Background()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	c := &CNPG{Dynamic: dyn, Kube: kubefake.NewClientset()}
	at := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	spec := ClusterSpec{Name: "db-copy", DatabaseID: "d2", ProjectID: "p1", Storage: "1Gi",
		Restore: &RestoreSource{Cluster: "db-main", TargetTime: &at}}
	if err := c.Create(ctx, "km-demo", spec); !errors.Is(err, ErrBackupsDisabled) {
		t.Fatalf("restauration sans sauvegardes configurées : %v", err)
	}
	c.Backup = &BackupTarget{Endpoint: "https://s3.example", Bucket: "db-backups", AccessKey: "a", SecretKey: "s", Retention: "7d", Schedule: "0 0 3 * * *"}
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

func TestImageVersion(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie": "18.6",
		"ghcr.io/cloudnative-pg/postgresql:17.6":               "17.6",
		"localhost:5000/pg":                                    "", "": "",
	} {
		if got := imageVersion(in); got != want {
			t.Errorf("%q → %q, attendu %q", in, got, want)
		}
	}
}
