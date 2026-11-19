package databases

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

type memImports struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.DatabaseImport
}

func (m *memImports) CreateImport(_ context.Context, i *domain.DatabaseImport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *i
	m.rows[i.ID] = &cp
	return nil
}
func (m *memImports) GetImport(_ context.Context, db, id uuid.UUID) (*domain.DatabaseImport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i, ok := m.rows[id]; ok && i.DatabaseID == db {
		cp := *i
		return &cp, nil
	}
	return nil, domain.ErrImportNotFound
}
func (m *memImports) GetImportByID(_ context.Context, id uuid.UUID) (*domain.DatabaseImport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i, ok := m.rows[id]; ok {
		cp := *i
		return &cp, nil
	}
	return nil, domain.ErrImportNotFound
}
func (m *memImports) ListImports(_ context.Context, db uuid.UUID, _ int) ([]domain.DatabaseImport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.DatabaseImport{}
	for _, i := range m.rows {
		if i.DatabaseID == db {
			out = append(out, *i)
		}
	}
	return out, nil
}
func (m *memImports) FinishImport(_ context.Context, id uuid.UUID, status, failure string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id].Status, m.rows[id].Failure, m.rows[id].FinishedAt = status, failure, &at
	return nil
}
func (m *memImports) SetImportStatus(_ context.Context, id uuid.UUID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id].Status = status
	return nil
}

type memObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func (m *memObjects) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return err
}
func (m *memObjects) Get(_ context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return readSeekCloser{bytes.NewReader(b)}, objectstore.ObjectInfo{Size: int64(len(b))}, nil
}
func (m *memObjects) Download(context.Context, string, io.Writer) error { return nil }
func (m *memObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
func (m *memObjects) DeletePrefix(context.Context, string) error { return nil }

type fakeImporter struct {
	started []ImportJobSpec
	state   ImportJobState
}

func (f *fakeImporter) StartImport(_ context.Context, _ string, s ImportJobSpec) error {
	f.started = append(f.started, s)
	return nil
}
func (f *fakeImporter) ImportState(context.Context, string, string) (ImportJobState, error) {
	return f.state, nil
}

var importNow = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func importSvc(t *testing.T) (*Service, *domain.Database, *fakeImporter, *memImports, *memObjects) {
	t.Helper()
	fc := &fakeClusters{state: ClusterState{Exists: true, ReadyInstance: true, Image: "ghcr.io/cloudnative-pg/postgresql:17.2"}}
	s, repo := svc(fc)
	d := &domain.Database{ID: uuid.New(), ProjectID: uuid.New(), Name: "main", Status: domain.StatusReady}
	repo.rows[d.ID] = d
	imp := &fakeImporter{}
	store := &memImports{rows: map[uuid.UUID]*domain.DatabaseImport{}}
	objs := &memObjects{data: map[string][]byte{}}
	s.Imports = &ImportConfig{Store: store, Dumps: objs, Signer: registry.Credentials{Secret: []byte("k")}, Importer: imp,
		PublicAPIURL: "http://10.42.0.1:18080/kmanager/v1/", FetchImage: "curl:1", EgressAllow: []string{"10.42.0.1/32"},
		Now: func() time.Time { return importNow }}
	return s, d, imp, store, objs
}

var dump = []byte("PGDMP\x01\x0e\x00rest-of-dump")

func TestImport_StartsJobWithSignedURL(t *testing.T) {
	ctx := context.Background()
	s, d, imp, store, objs := importSvc(t)
	got, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), true, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ImportRunning || !got.Clean || store.rows[got.ID].Status != domain.ImportRunning {
		t.Fatalf("import : %+v", got)
	}
	if !bytes.Equal(objs.data[dumpKey(got.ID)], dump) {
		t.Fatal("dump stocké tronqué (les 5 octets lus pour l'en-tête doivent suivre)")
	}
	spec := imp.started[0]
	if spec.Cluster != "db-main" || spec.Image != "ghcr.io/cloudnative-pg/postgresql:17.2" || !spec.Clean || spec.EgressAllow[0] != "10.42.0.1/32" {
		t.Fatalf("spec : %+v", spec)
	}
	u, err := url.Parse(spec.DumpURL)
	if err != nil || !strings.HasPrefix(spec.DumpURL, "http://10.42.0.1:18080/kmanager/v1/databases/imports/"+got.ID.String()+"/dump?") {
		t.Fatalf("URL : %s", spec.DumpURL)
	}
	exp, _ := strconv.ParseInt(u.Query().Get("expires"), 10, 64)

	// Le Job lit le dump avec la signature ; sans elle, rien.
	rc, size, err := s.Dump(ctx, got.ID, exp, u.Query().Get("sig"))
	if err != nil || size != int64(len(dump)) {
		t.Fatalf("dump : %v", err)
	}
	rc.Close()
	if _, _, err := s.Dump(ctx, got.ID, exp, "faux"); err == nil {
		t.Fatal("signature fausse acceptée")
	}
	if _, _, err := s.Dump(ctx, got.ID, importNow.Add(-time.Minute).Unix(), registry.Credentials{Secret: []byte("k")}.DumpSignature(got.ID, importNow.Add(-time.Minute).Unix())); err == nil {
		t.Fatal("URL expirée acceptée")
	}
}

func TestImport_Refusals(t *testing.T) {
	ctx := context.Background()
	s, d, imp, _, _ := importSvc(t)
	imp.state = ImportJobState{Exists: true} // Job lancé, pas fini
	if _, err := s.Import(ctx, d.ProjectID, d.ID, strings.NewReader("CREATE TABLE x();"), 17, false, "u"); err != domain.ErrNotADump {
		t.Fatalf("SQL brut : %v", err)
	}
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), domain.DefaultMaxDumpBytes+1, false, "u"); err != domain.ErrDumpTooLarge {
		t.Fatalf("trop gros : %v", err)
	}
	if _, err := s.Import(ctx, uuid.New(), d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u"); err != domain.ErrNotFound {
		t.Fatalf("autre projet : %v", err)
	}
	// Un import en cours : un second est refusé.
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u"); err != domain.ErrImportRunning {
		t.Fatalf("deuxième import : %v", err)
	}
	if len(imp.started) != 1 {
		t.Fatalf("Jobs : %d", len(imp.started))
	}
	// Base pas prête.
	s.Clusters.(*fakeClusters).state.ReadyInstance = false
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u"); err != domain.ErrNotReady {
		t.Fatalf("base pas prête : %v", err)
	}
	// Sans configuration : indisponible.
	s.Imports = nil
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u"); err != domain.ErrImportsDisabled {
		t.Fatalf("désactivé : %v", err)
	}
}

func TestImport_StateFollowsJob(t *testing.T) {
	ctx := context.Background()
	s, d, imp, _, objs := importSvc(t)
	got, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, "u")
	if err != nil {
		t.Fatal(err)
	}
	imp.state = ImportJobState{Exists: true}
	if v, _ := s.GetImport(ctx, d.ProjectID, d.ID, got.ID); v.Status != domain.ImportRunning {
		t.Fatalf("en cours : %+v", v)
	}
	imp.state = ImportJobState{Exists: true, Done: true, Failed: true, Log: `pg_restore: error: relation "users" already exists`}
	v, err := s.GetImport(ctx, d.ProjectID, d.ID, got.ID)
	if err != nil || v.Status != domain.ImportFailed || !strings.Contains(v.Failure, "already exists") || v.FinishedAt == nil {
		t.Fatalf("échec : %+v %v", v, err)
	}
	if _, ok := objs.data[dumpKey(got.ID)]; ok {
		t.Fatal("dump gardé après la fin")
	}
	// Terminé : un nouvel import est permis.
	imp.state = ImportJobState{Exists: true, Done: true}
	if _, err := s.Import(ctx, d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), true, "u"); err != nil {
		t.Fatalf("nouvel import : %v", err)
	}
	list, _ := s.ListImports(ctx, d.ProjectID, d.ID)
	if len(list) != 2 {
		t.Fatalf("liste : %+v", list)
	}
}

func TestImportJob_SafeDefaults(t *testing.T) {
	j := importJob("km-demo", ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg:17", FetchImage: "curl:1", DumpURL: "http://x/d", Clean: true})
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
	if err := c.StartImport(ctx, "km-demo", ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg", FetchImage: "curl", DumpURL: "u"}); err != nil {
		t.Fatal(err)
	}
	// Deuxième départ (reprise) : sans erreur.
	if err := c.StartImport(ctx, "km-demo", ImportJobSpec{ImportID: "i1", Cluster: "db-main", Image: "pg", FetchImage: "curl", DumpURL: "u"}); err != nil {
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
