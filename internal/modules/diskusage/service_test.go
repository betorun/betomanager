package diskusage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memStore struct {
	projects []Project
	rows     map[string]*Usage
}

func (m *memStore) ActiveProjects(context.Context) ([]Project, error) { return m.projects, nil }
func (m *memStore) Get(_ context.Context, ns, pvc string) (*Usage, error) {
	if u, ok := m.rows[ns+"/"+pvc]; ok {
		c := *u
		return &c, nil
	}
	return nil, nil
}
func (m *memStore) Save(_ context.Context, u *Usage) error {
	c := *u
	m.rows[u.Namespace+"/"+u.PVC] = &c
	return nil
}
func (m *memStore) Prune(_ context.Context, ns string, keep []string) error {
	k := map[string]bool{}
	for _, p := range keep {
		k[p] = true
	}
	for key, u := range m.rows {
		if u.Namespace == ns && !k[u.PVC] {
			delete(m.rows, key)
		}
	}
	return nil
}
func (m *memStore) ListProject(context.Context, uuid.UUID) ([]Usage, error) { return nil, nil }

type fakeMeasurer struct {
	disks []Disk
	used  map[string]int64
}

func (f *fakeMeasurer) Disks(context.Context, string) ([]Disk, error) { return f.disks, nil }
func (f *fakeMeasurer) Measure(_ context.Context, d Disk) (int64, error) {
	return f.used[d.PVC], nil
}

type fakeTargets struct {
	wl, db  *Target
	stopped []uuid.UUID
}

func (f *fakeTargets) Workload(context.Context, uuid.UUID, string) (*Target, error) { return f.wl, nil }
func (f *fakeTargets) Database(context.Context, uuid.UUID, string) (*Target, error) { return f.db, nil }
func (f *fakeTargets) StopWorkload(_ context.Context, id uuid.UUID) error {
	f.stopped = append(f.stopped, id)
	f.wl.Status = workloadStopped
	return nil
}

type sentAlert struct {
	level int
	kind  string
}
type fakeAlerts struct{ sent []sentAlert }

func (f *fakeAlerts) DiskUsage(_ context.Context, _, _ uuid.UUID, level int, kind, _ string, _, _ int64) {
	f.sent = append(f.sent, sentAlert{level, kind})
}

const gi = int64(1 << 30)

func setup(kind string) (*Service, *fakeMeasurer, *fakeTargets, *fakeAlerts, *time.Time) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	m := &fakeMeasurer{disks: []Disk{{Namespace: "ns", PVC: "api-pvc", Kind: kind, Name: "api", CapacityBytes: gi}}, used: map[string]int64{}}
	t := &fakeTargets{wl: &Target{ID: uuid.New(), Status: "RUNNING"}, db: &Target{ID: uuid.New(), Status: "READY"}}
	a := &fakeAlerts{}
	s := &Service{Store: &memStore{projects: []Project{{ID: uuid.New(), Namespace: "ns"}}, rows: map[string]*Usage{}},
		Measurer: m, Targets: t, Alerts: a, Now: func() time.Time { return now }}
	return s, m, t, a, &now
}

func TestLevelOf(t *testing.T) {
	assert.Equal(t, LevelOK, LevelOf(79, 100))
	assert.Equal(t, LevelWarning, LevelOf(80, 100))
	assert.Equal(t, LevelFull, LevelOf(100, 100))
	assert.Equal(t, LevelStop, LevelOf(120, 100))
	assert.Equal(t, LevelOK, LevelOf(500, 0), "taille inconnue : pas d'alerte")
}

func TestPass_AlertsOncePerThresholdGoingUp(t *testing.T) {
	s, m, tg, a, _ := setup(KindWorkload)
	ctx := context.Background()
	m.used["api-pvc"] = gi * 85 / 100
	s.Pass(ctx)
	s.Pass(ctx)
	m.used["api-pvc"] = gi
	s.Pass(ctx)
	m.used["api-pvc"] = gi / 2
	s.Pass(ctx)
	m.used["api-pvc"] = gi * 9 / 10
	s.Pass(ctx)
	assert.Equal(t, []sentAlert{{80, KindWorkload}, {100, KindWorkload}, {80, KindWorkload}}, a.sent)
	assert.Empty(t, tg.stopped)
}

func TestPass_StopsWorkloadAt120WithGrace(t *testing.T) {
	s, m, tg, a, now := setup(KindWorkload)
	ctx := context.Background()
	m.used["api-pvc"] = gi * 13 / 10
	s.Pass(ctx)
	require.Equal(t, []uuid.UUID{tg.wl.ID}, tg.stopped)
	assert.Equal(t, []sentAlert{{120, KindWorkload}}, a.sent, "une seule alerte, celle de l'arrêt")

	// Toujours arrêté : rien de plus.
	s.Pass(ctx)
	assert.Len(t, tg.stopped, 1)
	assert.Len(t, a.sent, 1)

	// Relancé par le client pour faire de la place : délai de grâce.
	tg.wl.Status = "RUNNING"
	*now = now.Add(time.Hour)
	s.Pass(ctx)
	assert.Len(t, tg.stopped, 1)

	// Délai passé, toujours plein : nouvel arrêt.
	*now = now.Add(StopGrace)
	s.Pass(ctx)
	assert.Len(t, tg.stopped, 2)
	assert.Len(t, a.sent, 2)
}

func TestPass_AlreadyStoppedByClientOnlyAlerts(t *testing.T) {
	s, m, tg, a, _ := setup(KindWorkload)
	tg.wl.Status = workloadStopped
	m.used["api-pvc"] = 2 * gi
	s.Pass(context.Background())
	assert.Empty(t, tg.stopped)
	assert.Equal(t, []sentAlert{{120, KindWorkload}}, a.sent)
}

func TestPass_DatabaseNeverStopped(t *testing.T) {
	s, m, tg, a, _ := setup(KindDatabase)
	m.used["api-pvc"] = 2 * gi
	s.Pass(context.Background())
	assert.Empty(t, tg.stopped)
	assert.Equal(t, []sentAlert{{120, KindDatabase}}, a.sent)
}

func TestPass_SkipsUnknownTargetAndPrunes(t *testing.T) {
	s, m, tg, a, _ := setup(KindWorkload)
	st := s.Store.(*memStore)
	st.rows["ns/old-pvc"] = &Usage{Namespace: "ns", PVC: "old-pvc"}
	tg.wl = nil
	m.used["api-pvc"] = 2 * gi
	s.Pass(context.Background())
	assert.Empty(t, a.sent)
	assert.Empty(t, st.rows, "disque inconnu non enregistré, disque disparu retiré")
}

func TestPass_NewTargetResetsHistory(t *testing.T) {
	s, m, tg, a, _ := setup(KindWorkload)
	ctx := context.Background()
	m.used["api-pvc"] = gi
	s.Pass(ctx)
	tg.wl = &Target{ID: uuid.New(), Status: "RUNNING"} // conteneur recréé sous le même nom
	s.Pass(ctx)
	assert.Equal(t, []sentAlert{{100, KindWorkload}, {100, KindWorkload}}, a.sent)
}
