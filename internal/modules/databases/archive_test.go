package databases

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

type memArchives struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.Archive
	fail bool
}

func (m *memArchives) SaveArchive(_ context.Context, a *domain.Archive) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("insert failed")
	}
	m.rows[a.ID] = *a
	return nil
}

func (m *memArchives) ExpiredArchives(_ context.Context, now time.Time) ([]domain.Archive, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Archive
	for _, a := range m.rows {
		if !a.ExpiresAt.After(now) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *memArchives) DeleteArchive(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func archiveSvc(t *testing.T) (*Service, *domain.Database, *fakeShell, *memArchives, *memObjects) {
	t.Helper()
	s, d, sh, _, objs, _ := resetSvc(t, 12)
	arch := &memArchives{rows: map[uuid.UUID]domain.Archive{}}
	s.Archives = arch
	return s, d, sh, arch, objs
}

// La copie finale est rangée, puis la base est supprimée ; rien n'est vidé.
func TestArchiveAndDelete_CopyThenDelete(t *testing.T) {
	ctx := context.Background()
	s, d, sh, arch, objs := archiveSvc(t)
	org := uuid.New()
	until := importNow.AddDate(0, 0, 30)

	require.NoError(t, s.ArchiveAndDelete(ctx, org, d.ProjectID, d.ID, domain.ArchiveReasonPurge, until))

	assert.Equal(t, 1, sh.dumps)
	assert.Zero(t, sh.resets)
	require.Len(t, arch.rows, 1)
	for _, a := range arch.rows {
		assert.Equal(t, org, a.OrganizationID)
		assert.Equal(t, "main", a.DatabaseName)
		assert.Equal(t, until, a.ExpiresAt)
		assert.Contains(t, a.ObjectKey, "database-archives/"+org.String()+"/")
		assert.Len(t, objs.data[a.ObjectKey], 15)
	}
	fc := s.Clusters.(*fakeClusters)
	assert.Equal(t, []string{"db-main"}, fc.deleted)
}

// Copie impossible : la base reste.
func TestArchiveAndDelete_DumpFailureKeepsDatabase(t *testing.T) {
	s, d, sh, arch, objs := archiveSvc(t)
	sh.dumpErr = errors.New("pg_dump: boom")

	require.Error(t, s.ArchiveAndDelete(context.Background(), uuid.New(), d.ProjectID, d.ID, domain.ArchiveReasonPurge, importNow))

	assert.Empty(t, arch.rows)
	assert.Empty(t, objs.data)
	assert.Empty(t, s.Clusters.(*fakeClusters).deleted)
}

// Copie arrivée mais non enregistrée : l'objet est effacé, la base reste.
func TestArchiveAndDelete_UnrecordedCopyKeepsDatabase(t *testing.T) {
	s, d, _, arch, objs := archiveSvc(t)
	arch.fail = true

	require.Error(t, s.ArchiveAndDelete(context.Background(), uuid.New(), d.ProjectID, d.ID, domain.ArchiveReasonPurge, importNow))

	assert.Empty(t, objs.data)
	assert.Empty(t, s.Clusters.(*fakeClusters).deleted)
}

// Sans stockage des copies finales : aucune suppression.
func TestArchiveAndDelete_NoArchiveStore(t *testing.T) {
	s, d, sh, _, _ := archiveSvc(t)
	s.Archives = nil
	assert.ErrorIs(t, s.ArchiveAndDelete(context.Background(), uuid.New(), d.ProjectID, d.ID, domain.ArchiveReasonPurge, importNow),
		ErrArchiveUnavailable)
	assert.Zero(t, sh.dumps)
}

func TestPurgeExpiredArchives(t *testing.T) {
	ctx := context.Background()
	s, d, _, arch, objs := archiveSvc(t)
	require.NoError(t, s.ArchiveAndDelete(ctx, uuid.New(), d.ProjectID, d.ID, domain.ArchiveReasonPurge, importNow.Add(-time.Minute)))
	require.Len(t, objs.data, 1)

	n, err := s.PurgeExpiredArchives(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, arch.rows)
	assert.Empty(t, objs.data)
}
