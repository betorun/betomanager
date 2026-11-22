package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
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
	return &dauth.Project{ID: uuid.MustParse(id), Name: "demo", Namespace: "km-demo", Status: p.status}, nil
}

type fakeClusters struct {
	created  []ClusterSpec
	deleted  []string
	state    ClusterState
	createEr error
	// hibernations : appels à Hibernate (true : arrêt).
	hibernations []bool
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
		return "", domain.ErrAppSecretMissing
	}
	return "postgresql://app:s3cret@db-main-rw.km-demo:5432/app", nil
}

func (f *fakeClusters) Hibernate(_ context.Context, _, _ string, on bool) error {
	f.hibernations = append(f.hibernations, on)
	f.state.Hibernated = on
	if on {
		f.state.ReadyInstance = false
	}
	return nil
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
	if v.Status != domain.StatusProvisioning || v.Host != "db-main-rw.km-demo.svc" || v.StorageSize != "1Gi" || v.Port != 5432 {
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
	s, repo := svc(&fakeClusters{createEr: domain.ErrOperatorMissing})
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

type fakeWorkloads struct {
	project uuid.UUID
	set     map[string]string
	waits   []string
}

func (f *fakeWorkloads) WaitFor(_ context.Context, _ uuid.UUID, target string) error {
	f.waits = append(f.waits, target)
	return nil
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
	if len(wl.waits) != 1 || !strings.HasSuffix(wl.waits[0], "-rw:5432") {
		t.Fatalf("le conteneur doit attendre la base au démarrage : %v", wl.waits)
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

func TestImageVersion(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie": "18.6",
		"ghcr.io/cloudnative-pg/postgresql:17.6":               "17.6",
		"localhost:5000/pg":                                    "", "": "",
	} {
		if got := ImageVersion(in); got != want {
			t.Errorf("%q → %q, attendu %q", in, got, want)
		}
	}
}

type fakeAttachments struct{ saved []domain.Attachment }

func (f *fakeAttachments) SaveAttachment(_ context.Context, a *domain.Attachment) error {
	f.saved = append(f.saved, *a)
	return nil
}
func (f *fakeAttachments) ListAttachments(_ context.Context, id uuid.UUID) ([]domain.Attachment, error) {
	out := []domain.Attachment{}
	for _, a := range f.saved {
		if a.DatabaseID == id {
			out = append(out, a)
		}
	}
	return out, nil
}

type namedWorkloads struct{ fakeWorkloads }

func (namedWorkloads) NameOf(context.Context, uuid.UUID) (string, error) { return "bomba-api", nil }

// Un branchement est tracé et la carte de la base le montre.
func TestAttachRecordsAttachment(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, _ := svc(fc)
	store := &fakeAttachments{}
	s.Attachments = store
	s.Workloads = &namedWorkloads{fakeWorkloads{project: p, set: map[string]string{}}}
	v, _ := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u")
	id := uuid.MustParse(v.ID)
	fc.state = ClusterState{Exists: true, ReadyInstance: true}
	if _, err := s.Attach(ctx, p, id, uuid.New(), "BOMBA_DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].WorkloadName != "bomba-api" || got.Attachments[0].Variable != "BOMBA_DATABASE_URL" {
		t.Fatalf("branchements : %+v", got.Attachments)
	}
}

func TestStopAndStart(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	fc := &fakeClusters{}
	s, repo := svc(fc)
	s.Workloads = &fakeWorkloads{project: p, set: map[string]string{}}
	v, _ := s.Create(ctx, p, domain.CreateDatabaseRequest{Name: "main"}, "u")
	id := uuid.MustParse(v.ID)
	// En création : pas d'arrêt.
	fc.state = ClusterState{Exists: true}
	if _, err := s.Stop(ctx, p, id); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("en création : %v", err)
	}
	if _, err := s.Start(ctx, p, id); !errors.Is(err, domain.ErrNotStopped) {
		t.Fatalf("relance d'une base en marche : %v", err)
	}
	fc.state = ClusterState{Exists: true, ReadyInstance: true, Image: "ghcr.io/cloudnative-pg/postgresql:17.6"}
	out, err := s.Stop(ctx, p, id)
	if err != nil || out.Status != domain.StatusStopped || repo.rows[id].Status != domain.StatusStopped {
		t.Fatalf("arrêt : %v %+v", err, out)
	}
	// Déjà arrêtée : rien de plus côté cluster.
	if _, err := s.Stop(ctx, p, id); err != nil || len(fc.hibernations) != 1 {
		t.Fatalf("second arrêt : %v %v", err, fc.hibernations)
	}
	// La liste garde STOPPED ; brancher ou lire le contenu est refusé clairement.
	if list, _ := s.List(ctx, p); list[0].Status != domain.StatusStopped {
		t.Fatalf("liste : %s", list[0].Status)
	}
	if _, err := s.Attach(ctx, p, id, uuid.New(), "DATABASE_URL"); !errors.Is(err, domain.ErrStopped) {
		t.Fatalf("branchement d'une base arrêtée : %v", err)
	}
	out, err = s.Start(ctx, p, id)
	if err != nil || out.Status != domain.StatusProvisioning || fc.hibernations[1] {
		t.Fatalf("relance : %v %+v %v", err, out, fc.hibernations)
	}
	// Instance de nouveau prête : READY.
	fc.state.ReadyInstance = true
	if got, _ := s.Get(ctx, p, id); got.Status != domain.StatusReady {
		t.Fatalf("après relance : %s", got.Status)
	}
	if _, err := s.Stop(ctx, uuid.New(), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("autre projet : %v", err)
	}
}
