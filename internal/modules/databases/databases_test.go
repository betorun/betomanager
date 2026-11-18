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
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{ClusterGVR: "ClusterList"})
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
