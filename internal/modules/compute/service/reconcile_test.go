package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

type reconStore struct {
	Store
	vps      []domain.VPS
	applied  map[uuid.UUID]string
	stale    map[uuid.UUID]bool // version changée entre-temps
	machines []domain.ProviderMachine
	runs     []domain.ReconcileRun
}

func (s *reconStore) ProviderVPS(context.Context, string) ([]domain.VPS, error) { return s.vps, nil }
func (s *reconStore) ApplyObserved(_ context.Context, id uuid.UUID, _ int64, state, ipv4, _ string, _ time.Time) (bool, error) {
	if s.stale[id] {
		return false, nil
	}
	s.applied[id] = state + " " + ipv4
	return true, nil
}
func (s *reconStore) ReplaceMachines(_ context.Context, _ string, ms []domain.ProviderMachine) error {
	s.machines = ms
	return nil
}
func (s *reconStore) Purge(context.Context, time.Time, time.Time, time.Time) error { return nil }
func (s *reconStore) BeginRun(_ context.Context, r *domain.ReconcileRun, _ time.Duration) (bool, error) {
	for _, run := range s.runs {
		if run.FinishedAt == nil {
			return false, nil
		}
	}
	s.runs = append(s.runs, *r)
	return true, nil
}
func (s *reconStore) SaveRun(_ context.Context, r *domain.ReconcileRun) error {
	s.runs = append(s.runs, *r)
	return nil
}

type fakeProvider struct {
	ComputeProvider
	vms []ProviderVM
	err error
}

func (f *fakeProvider) Name() string                               { return domain.ProviderOVH }
func (f *fakeProvider) List(context.Context) ([]ProviderVM, error) { return f.vms, f.err }

func TestReconcile(t *testing.T) {
	stopped, ipChanged, raced, suspended, busy, gone, creating, deleted := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	store := &reconStore{applied: map[uuid.UUID]string{}, stale: map[uuid.UUID]bool{raced: true}, vps: []domain.VPS{
		{ID: stopped, ProviderID: "p-stopped", State: domain.StateRunning, IPv4: "1.1.1.1"},
		{ID: ipChanged, ProviderID: "p-ip", State: domain.StateRunning, IPv4: "2.2.2.2"},
		{ID: raced, ProviderID: "p-raced", State: domain.StateStopped},
		{ID: suspended, ProviderID: "p-susp", State: domain.StateSuspended},
		{ID: busy, ProviderID: "p-busy", State: domain.StateRunning},
		{ID: gone, ProviderID: "p-gone", State: domain.StateRunning},
		{ID: creating, ProviderID: "p-new", State: domain.StateCreating},
		{ID: deleted, ProviderID: "p-del", State: domain.StateDeleted},
	}}
	prov := &fakeProvider{vms: []ProviderVM{
		{ProviderID: "p-stopped", State: domain.StateStopped},             // arrêté chez le fournisseur : corrigé, IP gardée
		{ProviderID: "p-ip", State: domain.StateRunning, IPv4: "9.9.9.9"}, // nouvelle adresse
		{ProviderID: "p-raced", State: domain.StateRunning},               // le client a agi entre-temps : rien écrit
		{ProviderID: "p-susp", State: domain.StateRunning},                // suspendu mais en marche : écart
		{ProviderID: "p-busy", State: domain.StateStopped, Busy: true},    // opération en cours : rien
		{ProviderID: "p-new", State: domain.StateRunning},                 // en création : le workflow s'en charge
		{ProviderID: "p-del", State: domain.StateRunning},                 // résilié mais toujours là : écart (payé !)
		{ProviderID: "p-orphan", State: domain.StateRunning},              // inconnu
	}}
	r := &Reconciler{Store: store, Provider: prov}
	run, err := r.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, map[uuid.UUID]string{stopped: "stopped 1.1.1.1", ipChanged: "running 9.9.9.9"}, store.applied)
	assert.Equal(t, 8, run.Seen)
	assert.Equal(t, 2, run.Updated)
	assert.Equal(t, 1, run.Orphans)
	assert.Equal(t, 2, run.Drifts, "suspendu en marche + résilié présent")
	assert.Equal(t, 1, run.Missing, "p-gone")
	assert.NotNil(t, run.FinishedAt)

	drift := map[string]string{}
	for _, m := range store.machines {
		drift[m.ProviderID] = m.Drift
		if m.ProviderID != "p-orphan" {
			assert.NotNil(t, m.VPSID, m.ProviderID)
		}
	}
	assert.Equal(t, domain.DriftOrphan, drift["p-orphan"])
	assert.Equal(t, domain.DriftState, drift["p-susp"])
	assert.Equal(t, domain.DriftState, drift["p-del"])
	assert.Equal(t, "", drift["p-new"])
	assert.Equal(t, "", drift["p-busy"])
	require.Len(t, store.runs, 2, "passe enregistrée au début et à la fin")
}

func TestReconcile_ProviderDown(t *testing.T) {
	store := &reconStore{applied: map[uuid.UUID]string{}}
	r := &Reconciler{Store: store, Provider: &fakeProvider{err: errors.New("503")}}
	run, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, run.Error, "503")
	assert.Nil(t, store.machines, "relevé précédent gardé")

	_, err = (&Reconciler{Store: store}).Run(context.Background())
	assert.ErrorIs(t, err, ErrNoProvider)
}

type reconBackups struct {
	BackupProvider
	read, aborted []string
}

func (b *reconBackups) Backups(_ context.Context, pid string) (*ProviderBackups, error) {
	b.read = append(b.read, pid)
	return &ProviderBackups{Automated: true}, nil
}
func (b *reconBackups) AbortStuck(_ context.Context, pid string, _ time.Duration) (bool, error) {
	b.aborted = append(b.aborted, pid)
	return true, nil
}

type savingStore struct {
	*reconStore
	saved map[uuid.UUID]bool
}

func (s *savingStore) Backups(context.Context, uuid.UUID) (*domain.Backups, error) { return nil, nil }
func (s *savingStore) SaveBackups(_ context.Context, b *domain.Backups) error {
	s.saved[b.VPSID] = true
	return nil
}

func TestReconcile_BackupsAndStuckSnapshots(t *testing.T) {
	free, busy, op := uuid.New(), uuid.New(), uuid.New()
	base := &reconStore{applied: map[uuid.UUID]string{}, vps: []domain.VPS{
		{ID: free, ProviderID: "p-free", State: domain.StateRunning},
		{ID: busy, ProviderID: "p-busy", State: domain.StateRunning},
		{ID: op, ProviderID: "p-op", State: domain.StateRunning, Operation: domain.OpSnapshotting},
	}}
	store := &savingStore{reconStore: base, saved: map[uuid.UUID]bool{}}
	b := &reconBackups{}
	r := &Reconciler{Store: store, Backups: b, Provider: &fakeProvider{vms: []ProviderVM{
		{ProviderID: "p-free", State: domain.StateRunning},
		{ProviderID: "p-busy", State: domain.StateRunning, Busy: true},
		{ProviderID: "p-op", State: domain.StateRunning},
	}}}
	r.Now = dueAt(t, free)
	_, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"p-busy"}, b.aborted, "opération en cours chez le fournisseur : contrôlée (abortSnapshot si bloquée)")
	assert.Equal(t, []string{"p-free"}, b.read, "relevé seulement des VPS sans opération")
	assert.True(t, store.saved[free])
}

// dueAt : une heure de passe où les sauvegardes de ce VPS sont relevées.
func dueAt(t *testing.T, id uuid.UUID) func() time.Time {
	t.Helper()
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * passEvery)
		if dueThisPass(id, at, backupsEvery) {
			return func() time.Time { return at }
		}
	}
	t.Fatal("aucune passe de l'heure ne relève ce VPS")
	return nil
}

// Chaque VPS est relevé une passe sur six (une fois par heure).
func TestDueThisPass_OncePerHour(t *testing.T) {
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for n := 0; n < 50; n++ {
		id, due := uuid.New(), 0
		for i := 0; i < 6; i++ {
			if dueThisPass(id, base.Add(time.Duration(i)*passEvery), backupsEvery) {
				due++
			}
		}
		assert.Equal(t, 1, due)
	}
}

type failingApply struct{ *reconStore }

func (s failingApply) ApplyObserved(_ context.Context, id uuid.UUID, v int64, state, ipv4, ipv6 string, at time.Time) (bool, error) {
	if ipv4 == "6.6.6.6" {
		return false, errors.New("db down")
	}
	return s.reconStore.ApplyObserved(context.Background(), id, v, state, ipv4, ipv6, at)
}

// Un VPS illisible ou une écriture en échec n'arrêtent pas la passe ; le
// relevé est quand même remplacé et l'illisible n'est pas « absent ».
func TestReconcile_PartialFailures(t *testing.T) {
	bad, good, unread := uuid.New(), uuid.New(), uuid.New()
	base := &reconStore{applied: map[uuid.UUID]string{}, vps: []domain.VPS{
		{ID: bad, ProviderID: "p-bad", State: domain.StateRunning},
		{ID: good, ProviderID: "p-good", State: domain.StateRunning},
		{ID: unread, ProviderID: "p-unread", State: domain.StateRunning},
	}}
	r := &Reconciler{Store: failingApply{base}, Provider: &fakeProvider{vms: []ProviderVM{
		{ProviderID: "p-bad", State: domain.StateRunning, IPv4: "6.6.6.6"},
		{ProviderID: "p-good", State: domain.StateStopped},
		{ProviderID: "p-unread", Unread: true},
	}}}
	run, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Equal(t, "stopped ", base.applied[good])
	assert.Equal(t, 0, run.Missing)
	assert.Equal(t, 1, run.Unread)
	require.Len(t, base.machines, 3)
	assert.True(t, base.machines[2].Keep)
	assert.NotNil(t, base.runs[len(base.runs)-1].FinishedAt, "passe close malgré l'échec")
}

// Une seule passe à la fois.
func TestReconcile_SingleFlight(t *testing.T) {
	store := &reconStore{applied: map[uuid.UUID]string{}, runs: []domain.ReconcileRun{{ID: uuid.New()}}}
	_, err := (&Reconciler{Store: store, Provider: &fakeProvider{}}).Run(context.Background())
	assert.ErrorIs(t, err, ErrReconcileRunning)
}
