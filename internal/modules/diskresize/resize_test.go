package diskresize

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

type memStore struct{ rows []*Resize }

func (m *memStore) Create(_ context.Context, r *Resize) error {
	c := *r
	m.rows = append(m.rows, &c)
	return nil
}

func (m *memStore) Save(_ context.Context, r *Resize) error {
	for i, x := range m.rows {
		if x.ID == r.ID {
			c := *r
			m.rows[i] = &c
			return nil
		}
	}
	return errors.New("not found")
}

func (m *memStore) Last(_ context.Context, w uuid.UUID) (*Resize, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.rows[i].WorkloadID == w {
			c := *m.rows[i]
			return &c, nil
		}
	}
	return nil, nil
}

type fakeWorkloads struct {
	w          Workload
	calls      []string
	used       int64
	limit      int64
	failApply  error
	appliedSiz string
}

func (f *fakeWorkloads) Get(_ context.Context, id uuid.UUID) (*Workload, error) {
	if id != f.w.ID {
		return nil, nil
	}
	c := f.w
	return &c, nil
}
func (f *fakeWorkloads) Stop(context.Context, uuid.UUID) error {
	f.calls = append(f.calls, "stop")
	f.w.Status = "STOPPED"
	return nil
}
func (f *fakeWorkloads) Start(context.Context, uuid.UUID) error {
	f.calls = append(f.calls, "start")
	f.w.Status = "RUNNING"
	return nil
}
func (f *fakeWorkloads) ApplySize(_ context.Context, _ uuid.UUID, size string) error {
	f.calls = append(f.calls, "apply "+size)
	if f.failApply != nil {
		return f.failApply
	}
	f.w.StorageSize, f.w.Status, f.appliedSiz = size, "RUNNING", size
	return nil
}
func (f *fakeWorkloads) Storage(context.Context, uuid.UUID) (int64, int64, error) {
	return f.used, f.limit, nil
}

// fakeClaims : disques en mémoire (nom → taille, contenu).
type fakeClaims struct {
	size     map[string]string
	data     map[string]string
	calls    []string
	failCopy map[string]error // clé : "from>to"
	failRec  error
}

func (f *fakeClaims) WaitDetached(context.Context, string, string) error { return nil }
func (f *fakeClaims) Size(_ context.Context, _, c string) (string, error) {
	return f.size[c], nil
}
func (f *fakeClaims) CreateTemp(_ context.Context, _, temp, _, size string) error {
	f.calls = append(f.calls, "temp "+size)
	f.size[temp], f.data[temp] = size, ""
	return nil
}
func (f *fakeClaims) Recreate(_ context.Context, _, claim, temp, size string) error {
	f.calls = append(f.calls, "recreate "+size)
	if _, ok := f.size[temp]; !ok {
		return errors.New("temp missing")
	}
	if f.failRec != nil {
		return f.failRec
	}
	f.size[claim], f.data[claim] = size, ""
	return nil
}
func (f *fakeClaims) Copy(_ context.Context, _, from, to string, _, _ int64) error {
	f.calls = append(f.calls, "copy "+from+">"+to)
	if err := f.failCopy[from+">"+to]; err != nil {
		return err
	}
	f.data[to] = f.data[from]
	return nil
}
func (f *fakeClaims) Delete(_ context.Context, _, c string) error {
	if _, ok := f.size[c]; ok {
		f.calls = append(f.calls, "delete "+c)
	}
	delete(f.size, c)
	delete(f.data, c)
	return nil
}

func setup(status string) (*Service, *fakeWorkloads, *fakeClaims, *memStore) {
	id, project := uuid.New(), uuid.New()
	wl := &fakeWorkloads{w: Workload{ID: id, ProjectID: project, Name: "api", Namespace: "km-x", Persistent: true,
		StorageSize: "1Gi", Status: status, UID: 1000, GID: 2000}, used: 1 << 30, limit: 10 << 30}
	cl := &fakeClaims{size: map[string]string{"api-pvc": "1Gi"}, data: map[string]string{"api-pvc": "DATA"}, failCopy: map[string]error{}}
	st := &memStore{}
	return &Service{Store: st, Workloads: wl, Claims: cl, Sync: true}, wl, cl, st
}

func TestResize_CopiesSwapsAndRestarts(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	r, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u")
	if err != nil {
		t.Fatal(err)
	}
	got := st.rows[0]
	if got.Status != StatusCompleted || got.Phase != PhaseDone || r.FromSize != "1Gi" {
		t.Fatalf("resize = %+v", got)
	}
	if cl.size["api-pvc"] != "5Gi" || cl.data["api-pvc"] != "DATA" {
		t.Fatalf("disk = %s %q", cl.size["api-pvc"], cl.data["api-pvc"])
	}
	if _, ok := cl.size["api-pvc-resize"]; ok {
		t.Fatal("temporary disk kept")
	}
	want := []string{"temp 5Gi", "copy api-pvc>api-pvc-resize", "recreate 5Gi", "copy api-pvc-resize>api-pvc", "delete api-pvc-resize"}
	if !slices.Equal(cl.calls, want) {
		t.Fatalf("claims calls = %v", cl.calls)
	}
	if !slices.Equal(wl.calls, []string{"stop", "apply 5Gi"}) {
		t.Fatalf("workload calls = %v", wl.calls)
	}
}

func TestResize_StoppedContainerStaysStopped(t *testing.T) {
	s, wl, _, _ := setup("STOPPED")
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "2Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wl.calls, []string{"apply 2Gi", "stop"}) {
		t.Fatalf("workload calls = %v", wl.calls)
	}
}

func TestResize_CopyFailureRollsBack(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	cl.failCopy["api-pvc>api-pvc-resize"] = errors.New("copy mismatch")
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	got := st.rows[0]
	if got.Status != StatusFailed || got.Phase != PhaseCopying || got.Resumable() {
		t.Fatalf("resize = %+v", got)
	}
	if cl.size["api-pvc"] != "1Gi" || cl.data["api-pvc"] != "DATA" {
		t.Fatal("original disk touched")
	}
	if _, ok := cl.size["api-pvc-resize"]; ok {
		t.Fatal("temporary disk kept after a rollback")
	}
	if !slices.Equal(wl.calls, []string{"stop", "start"}) {
		t.Fatalf("workload calls = %v", wl.calls)
	}
	// Nouvelle demande : nouvelle opération.
	delete(cl.failCopy, "api-pvc>api-pvc-resize")
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 2 || st.rows[1].Status != StatusCompleted {
		t.Fatalf("rows = %+v", st.rows)
	}
}

func TestResize_FailureAfterSwapResumes(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	cl.failCopy["api-pvc-resize>api-pvc"] = errors.New("node gone")
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	got := st.rows[0]
	if got.Status != StatusFailed || got.Phase != PhaseRestoring || !got.Resumable() {
		t.Fatalf("resize = %+v", got)
	}
	if cl.data["api-pvc-resize"] != "DATA" {
		t.Fatal("temporary disk lost")
	}
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "10Gi", "user:u"); !errors.Is(err, ErrResumeSize) {
		t.Fatalf("other size: err = %v", err)
	}
	delete(cl.failCopy, "api-pvc-resize>api-pvc")
	cl.calls = nil
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "", "user:u"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 || st.rows[0].Status != StatusCompleted {
		t.Fatalf("rows = %+v", st.rows)
	}
	// Le disque d'origine n'est jamais supprimé deux fois.
	if !slices.Equal(cl.calls, []string{"copy api-pvc-resize>api-pvc", "delete api-pvc-resize"}) {
		t.Fatalf("claims calls = %v", cl.calls)
	}
	if cl.data["api-pvc"] != "DATA" || cl.size["api-pvc"] != "5Gi" {
		t.Fatal("data not restored")
	}
}

func TestResize_SwapResumeSkipsRecreatedDisk(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	cl.failRec = errors.New("api down")
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Phase != PhaseSwapping || !st.rows[0].Resumable() {
		t.Fatalf("resize = %+v", st.rows[0])
	}
	cl.failRec = nil
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Status != StatusCompleted || cl.data["api-pvc"] != "DATA" {
		t.Fatalf("resize = %+v, data %q", st.rows[0], cl.data["api-pvc"])
	}
}

func TestResize_Refusals(t *testing.T) {
	s, wl, _, _ := setup("RUNNING")
	ctx := context.Background()
	for _, size := range []string{"1Gi", "512Mi", "3Gi", "", "50Gi"} {
		if _, err := s.Start(ctx, wl.w.ProjectID, wl.w.ID, size, "u"); !errors.Is(err, ErrInvalidSize) {
			t.Fatalf("%q: err = %v", size, err)
		}
	}
	wl.limit = 4 << 30
	if _, err := s.Start(ctx, wl.w.ProjectID, wl.w.ID, "5Gi", "u"); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: err = %v", err)
	}
	if _, err := s.Start(ctx, uuid.New(), wl.w.ID, "2Gi", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other project: err = %v", err)
	}
	wl.w.Persistent = false
	if _, err := s.Start(ctx, wl.w.ProjectID, wl.w.ID, "2Gi", "u"); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("no disk: err = %v", err)
	}
}

func TestResize_InterruptedIsResumable(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	// Coupée par un redémarrage, en pleine recopie.
	cl.size["api-pvc-resize"], cl.data["api-pvc-resize"] = "5Gi", "DATA"
	cl.size["api-pvc"], cl.data["api-pvc"] = "5Gi", ""
	st.rows = append(st.rows, &Resize{ID: uuid.New(), WorkloadID: wl.w.ID, ProjectID: wl.w.ProjectID, FromSize: "1Gi", ToSize: "5Gi",
		Phase: PhaseRestoring, Status: StatusRunning})
	last, err := s.Last(context.Background(), wl.w.ProjectID, wl.w.ID)
	if err != nil || last.Status != StatusFailed || !last.CanResume {
		t.Fatalf("last = %+v, %v", last, err)
	}
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "", "u"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Status != StatusCompleted || cl.data["api-pvc"] != "DATA" {
		t.Fatalf("resize = %+v", st.rows[0])
	}
}

func TestBytesOf(t *testing.T) {
	if bytesOf("5Gi") != 5<<30 || bytesOf("512Mi") != 512<<20 || bytesOf("x") != 0 {
		t.Fatal("bytesOf")
	}
}
