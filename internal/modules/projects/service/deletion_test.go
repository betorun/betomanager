package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// fakeDeletionStore : suppression programmée en mémoire, mêmes transitions
// conditionnelles que la base.
type fakeDeletionStore struct {
	p       *authdomain.Project
	vps     int64
	stopped *authdomain.DeletionStopped
}

func (f *fakeDeletionStore) ScheduleDeletion(_ context.Context, _ uuid.UUID, at time.Time, by string) (bool, error) {
	if f.p.Status != utils.ProjectStatusReady && f.p.Status != utils.ProjectStatusError {
		return false, nil
	}
	f.p.Status, f.p.DeletionScheduledAt, f.p.DeletionRequestedBy = utils.ProjectStatusPendingDeletion, &at, by
	return true, nil
}
func (f *fakeDeletionStore) SetDeletionStopped(_ context.Context, _ uuid.UUID, s authdomain.DeletionStopped) error {
	f.stopped = &s
	return nil
}
func (f *fakeDeletionStore) CancelDeletion(context.Context, uuid.UUID) (*authdomain.DeletionStopped, bool, error) {
	if f.p.Status != utils.ProjectStatusPendingDeletion {
		return nil, false, nil
	}
	f.p.Status, f.p.DeletionScheduledAt = utils.ProjectStatusReady, nil
	s := f.stopped
	f.stopped = nil
	return s, true, nil
}
func (f *fakeDeletionStore) DueDeletions(_ context.Context, now time.Time, _ int) ([]authdomain.Project, error) {
	if f.p.Status == utils.ProjectStatusPendingDeletion && !f.p.DeletionScheduledAt.After(now) {
		return []authdomain.Project{*f.p}, nil
	}
	return nil, nil
}
func (f *fakeDeletionStore) MarkDeleting(context.Context, uuid.UUID) (bool, error) {
	if f.p.Status != utils.ProjectStatusPendingDeletion {
		return false, nil
	}
	f.p.Status = utils.ProjectStatusDeleting
	return true, nil
}
func (f *fakeDeletionStore) ActiveVPS(context.Context, uuid.UUID) (int64, error) { return f.vps, nil }
func (f *fakeDeletionStore) PendingDeletion(context.Context, uuid.UUID) (bool, error) {
	return f.p.Status == utils.ProjectStatusPendingDeletion, nil
}

type fakeContainers struct {
	running, started []uuid.UUID
}

func (f *fakeContainers) StopRunning(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	out := f.running
	f.running = nil
	return out, nil
}
func (f *fakeContainers) Start(_ context.Context, id uuid.UUID) error {
	f.started = append(f.started, id)
	return nil
}

type fakeDatabases struct {
	running, started []uuid.UUID
}

func (f *fakeDatabases) StopRunning(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	out := f.running
	f.running = nil
	return out, nil
}
func (f *fakeDatabases) Start(_ context.Context, _, id uuid.UUID) error {
	f.started = append(f.started, id)
	return nil
}

type fakeDeletionNotifier struct{ scheduled, restored int }

func (f *fakeDeletionNotifier) ProjectDeletionScheduled(context.Context, uuid.UUID, string, time.Time, string) {
	f.scheduled++
}
func (f *fakeDeletionNotifier) ProjectRestored(context.Context, uuid.UUID, string, string) {
	f.restored++
}

func graceFixture(t *testing.T) (*ProjectService, *fakeDeletionStore, *fakeDistributor, *fakeContainers, *fakeDatabases, *fakeDeletionNotifier, *time.Time) {
	t.Helper()
	p := &authdomain.Project{ID: uuid.New(), Name: "diclauver", Namespace: "km-abc", Status: utils.ProjectStatusReady}
	store := &fakeDeletionStore{p: p}
	dist := &fakeDistributor{handle: temporal.WorkflowHandle{WorkflowID: "wf-del"}}
	svc := newProjectServiceForTest(dist, &fakeProjectRepo{project: p})
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	cont := &fakeContainers{running: []uuid.UUID{uuid.New(), uuid.New()}}
	dbs := &fakeDatabases{running: []uuid.UUID{uuid.New()}}
	notif := &fakeDeletionNotifier{}
	svc.Deletion, svc.Containers, svc.ProjectDatabases, svc.Notifier = store, cont, dbs, notif
	svc.Now = func() time.Time { return now }
	return svc, store, dist, cont, dbs, notif, &now
}

// Demande : rien n'est effacé, tout est arrêté, effacement dans 12 h.
func TestDeleteProject_SchedulesWithGrace(t *testing.T) {
	svc, store, dist, cont, dbs, notif, now := graceFixture(t)
	id := store.p.ID.String()
	running, dbRunning := cont.running, dbs.running

	resp, err := svc.DeleteProject(context.Background(), id, "user:awa")
	require.NoError(t, err)
	assert.Equal(t, utils.ProjectStatusPendingDeletion, resp.Status)
	require.NotNil(t, resp.DeletionScheduledAt)
	assert.Equal(t, now.Add(12*time.Hour), *resp.DeletionScheduledAt)
	assert.Nil(t, dist.gotWorkflow, "aucun effacement avant l'échéance")
	assert.Equal(t, running, store.stopped.Workloads)
	assert.Equal(t, dbRunning, store.stopped.Databases)
	assert.Equal(t, "user:awa", store.p.DeletionRequestedBy)
	assert.Equal(t, 1, notif.scheduled)

	// Deuxième demande : même échéance, rien de plus.
	resp, err = svc.DeleteProject(context.Background(), id, "user:awa")
	require.NoError(t, err)
	assert.Equal(t, now.Add(12*time.Hour), *resp.DeletionScheduledAt)
	assert.Equal(t, 1, notif.scheduled)

	// Avant l'échéance, la purge ne touche à rien.
	*now = now.Add(11 * time.Hour)
	svc.PurgeDue(context.Background())
	assert.Nil(t, dist.gotWorkflow)
	assert.Equal(t, utils.ProjectStatusPendingDeletion, store.p.Status)
}

// Restauration : tout ce qui avait été arrêté redémarre, et la purge ne
// passe plus.
func TestRestoreProject_RestartsWhatWasStopped(t *testing.T) {
	svc, store, dist, cont, dbs, notif, now := graceFixture(t)
	id := store.p.ID.String()
	running, dbRunning := cont.running, dbs.running
	_, err := svc.DeleteProject(context.Background(), id, "user:awa")
	require.NoError(t, err)

	_, err = svc.RestoreProject(context.Background(), id, "user:awa")
	require.NoError(t, err)
	assert.Equal(t, utils.ProjectStatusReady, store.p.Status)
	assert.Equal(t, running, cont.started)
	assert.Equal(t, dbRunning, dbs.started)
	assert.Equal(t, 1, notif.restored)

	*now = now.Add(13 * time.Hour)
	svc.PurgeDue(context.Background())
	assert.Nil(t, dist.gotWorkflow, "projet restauré : jamais effacé")

	_, err = svc.RestoreProject(context.Background(), id, "user:awa")
	assert.ErrorIs(t, err, ErrProjectNotPendingDeletion)
}

// Échéance passée : la purge lance l'effacement définitif, une seule fois.
func TestPurgeDue_DeletesAfterGrace(t *testing.T) {
	svc, store, dist, _, _, _, now := graceFixture(t)
	_, err := svc.DeleteProject(context.Background(), store.p.ID.String(), "user:awa")
	require.NoError(t, err)

	*now = now.Add(12*time.Hour + time.Minute)
	svc.PurgeDue(context.Background())
	require.NotNil(t, dist.gotWorkflow)
	assert.Equal(t, utils.ProjectStatusDeleting, store.p.Status)

	// Trop tard pour restaurer.
	_, err = svc.RestoreProject(context.Background(), store.p.ID.String(), "user:awa")
	assert.ErrorIs(t, err, ErrProjectNotPendingDeletion)
}

// VPS liés : refus avant d'arrêter quoi que ce soit.
func TestDeleteProject_RefusedWithVPS(t *testing.T) {
	svc, store, _, cont, _, _, _ := graceFixture(t)
	store.vps = 1
	_, err := svc.DeleteProject(context.Background(), store.p.ID.String(), "user:awa")
	assert.ErrorIs(t, err, ErrProjectHasVPS)
	assert.Equal(t, utils.ProjectStatusReady, store.p.Status)
	assert.Len(t, cont.running, 2, "rien n'est arrêté")
}

// Projet en création : pas de suppression programmée.
func TestDeleteProject_NotReady(t *testing.T) {
	svc, store, _, _, _, _, _ := graceFixture(t)
	store.p.Status = utils.ProjectStatusProvisioning
	_, err := svc.DeleteProject(context.Background(), store.p.ID.String(), "user:awa")
	assert.ErrorIs(t, err, ErrProjectNotDeletable)
}
