package images

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
)

const host = "r.example"

type fakeStore struct {
	rows   []domain.Build
	inUse  []string
	pruned []uuid.UUID
	sizes  map[uuid.UUID]int64
	usage  int64
	// usageAfter : usage une fois des images retirées.
	usageAfter *int64
}

func (f *fakeStore) SetImageSize(_ context.Context, id uuid.UUID, size int64) error {
	f.sizes[id] = size
	return nil
}
func (f *fakeStore) Images(context.Context, uuid.UUID, string) ([]domain.Build, error) {
	return f.rows, nil
}
func (f *fakeStore) MarkPruned(_ context.Context, id uuid.UUID) error {
	f.pruned = append(f.pruned, id)
	return nil
}
func (f *fakeStore) Usage(context.Context, uuid.UUID) (int64, error) {
	if f.usageAfter != nil && len(f.pruned) > 0 {
		return *f.usageAfter, nil
	}
	return f.usage, nil
}
func (f *fakeStore) Names(context.Context, uuid.UUID) ([]string, error)          { return []string{"web"}, nil }
func (f *fakeStore) WorkloadImages(context.Context, uuid.UUID) ([]string, error) { return f.inUse, nil }

type fakeRegistry struct {
	deleted []string
	size    int64
}

func (f *fakeRegistry) ImageSize(context.Context, uuid.UUID, string, string) (int64, error) {
	return f.size, nil
}
func (f *fakeRegistry) Delete(_ context.Context, _ uuid.UUID, repo, digest string) error {
	f.deleted = append(f.deleted, repo+"@"+digest)
	return nil
}

func build(project uuid.UUID, i int, digest string) domain.Build {
	id := uuid.New()
	finished := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)
	return domain.Build{ID: id, ProjectID: project, Name: "web", Status: domain.StatusSucceeded, Digest: digest,
		Image: host + "/" + project.String() + "/web:b-" + id.String()[:8], FinishedAt: &finished}
}

func TestPrune_KeepsRecentAndInUse(t *testing.T) {
	project := uuid.New()
	var rows []domain.Build
	for i := range 6 {
		rows = append(rows, build(project, i, "sha256:d"+string(rune('0'+i))))
	}
	store := &fakeStore{rows: rows, inUse: []string{rows[4].Image}, sizes: map[uuid.UUID]int64{}}
	reg := &fakeRegistry{}
	k := &Keeper{Store: store, Registry: reg, Host: host, Keep: 2}

	require.NoError(t, k.Prune(context.Background(), project, "web"))

	repo := project.String() + "/web"
	assert.Equal(t, []string{repo + "@sha256:d2", repo + "@sha256:d3", repo + "@sha256:d5"}, reg.deleted,
		"les 2 plus récentes et celle en ligne (d4) restent")
	assert.Equal(t, []uuid.UUID{rows[2].ID, rows[3].ID, rows[5].ID}, store.pruned)
}

// Même contenu qu'une image gardée : supprimer le digest l'effacerait aussi.
func TestPrune_SharedDigestNotDeleted(t *testing.T) {
	project := uuid.New()
	rows := []domain.Build{build(project, 0, "sha256:same"), build(project, 1, "sha256:same"), build(project, 2, "sha256:old"), build(project, 3, "sha256:old")}
	store := &fakeStore{rows: rows, sizes: map[uuid.UUID]int64{}}
	reg := &fakeRegistry{}
	k := &Keeper{Store: store, Registry: reg, Host: host, Keep: 1}

	require.NoError(t, k.Prune(context.Background(), project, "web"))

	assert.Equal(t, []string{project.String() + "/web@sha256:old"}, reg.deleted, "un seul appel par digest")
	assert.Len(t, store.pruned, 3)
}

func TestAfterBuild_RecordsSize(t *testing.T) {
	project := uuid.New()
	b := build(project, 0, "sha256:x")
	store := &fakeStore{rows: []domain.Build{b}, sizes: map[uuid.UUID]int64{}}
	k := &Keeper{Store: store, Registry: &fakeRegistry{size: 1234}, Host: host}

	k.AfterBuild(context.Background(), &b)

	assert.Equal(t, int64(1234), store.sizes[b.ID])
}

func TestCheckQuota(t *testing.T) {
	store := &fakeStore{usage: 10}
	k := &Keeper{Store: store, Quota: 10}
	assert.ErrorIs(t, k.CheckQuota(context.Background(), uuid.New()), domain.ErrImageQuota)
	store.usage = 9
	assert.NoError(t, k.CheckQuota(context.Background(), uuid.New()))
	assert.NoError(t, (&Keeper{Store: &fakeStore{usage: 1 << 40}, Quota: -1}).CheckQuota(context.Background(), uuid.New()), "sans quota")
	var none *Keeper
	assert.NoError(t, none.CheckQuota(context.Background(), uuid.New()))
}

// Builds d'un ancien registre : ni supprimés ni marqués.
func TestPrune_IgnoresOtherRegistryHost(t *testing.T) {
	project := uuid.New()
	legacy := build(project, 5, "sha256:legacy")
	legacy.Image = "172.21.0.1:5000/" + project.String() + "/web:b-old"
	store := &fakeStore{rows: []domain.Build{build(project, 0, "sha256:a"), legacy}, sizes: map[uuid.UUID]int64{}}
	reg := &fakeRegistry{}
	k := &Keeper{Store: store, Registry: reg, Host: host, Keep: 1}

	require.NoError(t, k.Prune(context.Background(), project, "web"))
	assert.Empty(t, reg.deleted)
	assert.Empty(t, store.pruned)
}

// Quota atteint : les anciennes versions sont retirées avant de refuser.
func TestCheckQuota_CompactsFirst(t *testing.T) {
	project := uuid.New()
	after := int64(3)
	store := &fakeStore{rows: []domain.Build{build(project, 0, "sha256:a"), build(project, 1, "sha256:b")},
		usage: 10, usageAfter: &after, sizes: map[uuid.UUID]int64{}}
	reg := &fakeRegistry{}
	k := &Keeper{Store: store, Registry: reg, Host: host, Quota: 10}

	require.NoError(t, k.CheckQuota(context.Background(), project))
	assert.Equal(t, []string{project.String() + "/web@sha256:b"}, reg.deleted, "seule la dernière reste")
}

func TestUsage(t *testing.T) {
	used, quota, err := (&Keeper{Store: &fakeStore{usage: 7}, Quota: 10}).Usage(context.Background(), uuid.New())
	assert.NoError(t, err)
	assert.Equal(t, int64(7), used)
	assert.Equal(t, int64(10), quota)

	_, quota, _ = (&Keeper{Store: &fakeStore{}}).Usage(context.Background(), uuid.New())
	assert.Equal(t, DefaultQuota, quota, "quota par défaut")

	_, quota, _ = (&Keeper{Store: &fakeStore{}, Quota: -1}).Usage(context.Background(), uuid.New())
	assert.Zero(t, quota, "sans quota")
}

func TestRemove(t *testing.T) {
	project := uuid.New()
	a, b, c := build(project, 0, "sha256:a"), build(project, 1, "sha256:b"), build(project, 2, "sha256:a")
	repo := project.String() + "/web"

	t.Run("deletes an unused image", func(t *testing.T) {
		store, reg := &fakeStore{rows: []domain.Build{a, b}}, &fakeRegistry{}
		k := &Keeper{Store: store, Registry: reg, Host: host}
		require.NoError(t, k.Remove(context.Background(), &b))
		assert.Equal(t, []string{repo + "@sha256:b"}, reg.deleted)
		assert.Equal(t, []uuid.UUID{b.ID}, store.pruned)
	})
	t.Run("refused while a container runs it", func(t *testing.T) {
		store, reg := &fakeStore{rows: []domain.Build{a, b}, inUse: []string{b.Image}}, &fakeRegistry{}
		k := &Keeper{Store: store, Registry: reg, Host: host}
		require.ErrorIs(t, k.Remove(context.Background(), &b), domain.ErrImageInUse)
		assert.Empty(t, reg.deleted)
	})
	t.Run("digest shared with a kept image stays in the registry", func(t *testing.T) {
		store, reg := &fakeStore{rows: []domain.Build{a, b, c}}, &fakeRegistry{}
		k := &Keeper{Store: store, Registry: reg, Host: host}
		require.NoError(t, k.Remove(context.Background(), &c))
		assert.Empty(t, reg.deleted)
		assert.Equal(t, []uuid.UUID{c.ID}, store.pruned)
	})
	t.Run("no image", func(t *testing.T) {
		gone := b
		gone.ImagePrunedAt = b.FinishedAt
		k := &Keeper{Store: &fakeStore{}, Registry: &fakeRegistry{}, Host: host}
		require.ErrorIs(t, k.Remove(context.Background(), &gone), domain.ErrNoImage)
		failed := build(project, 3, "")
		failed.Status = domain.StatusFailed
		require.ErrorIs(t, k.Remove(context.Background(), &failed), domain.ErrNoImage)
	})
}
