package addons

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
)

// stopSpawner : Spawner de test dont l'arrêt est piloté par le test.
type stopSpawner struct {
	stop context.Context
	wg   sync.WaitGroup
}

func (s *stopSpawner) Go(_ string, fn func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.stop)
	}()
}

// blockingRunner : la copie ne finit qu'à l'annulation de son contexte.
type blockingRunner struct {
	fakeRunner
	started chan struct{}
}

func (b *blockingRunner) Dump(ctx context.Context, _ BackupTask) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

// Arrêt de l'API pendant une sauvegarde manuelle : elle s'interrompt et
// l'arrêt attend qu'elle ait enregistré « interrompue » (avant, la ligne
// restait « en cours » et la base pouvait être fermée sous elle).
func TestBackup_ShutdownRecordsInterruption(t *testing.T) {
	s, _, store, _, project, id := backupService(t)
	runner := &blockingRunner{started: make(chan struct{})}
	s.Backups.Runner = runner
	stop, shutdown := context.WithCancel(context.Background())
	sp := &stopSpawner{stop: stop}
	s.Spawner = sp

	b, err := s.StartBackup(context.Background(), project, id, "user:1")
	require.NoError(t, err)
	<-runner.started
	shutdown()
	sp.wg.Wait() // ce que fait l'arrêt de l'application

	store.mu.Lock()
	got := store.rows[b.ID]
	store.mu.Unlock()
	assert.Equal(t, domain.BackupFailed, got.Status)
	assert.Equal(t, domain.ErrBackupInterrupted.Error(), got.Error)
	assert.NotNil(t, got.FinishedAt)
}

// Process tué net (pas d'arrêt propre) : la ligne reste « en cours ». Passé
// le délai d'une copie, la liste la montre interrompue au lieu de « en
// cours » pendant la semaine qui précède sa purge.
func TestBackup_StaleRunningListedAsInterrupted(t *testing.T) {
	s, _, store, _, project, id := backupService(t)
	now := time.Now()
	s.Backups.Now = func() time.Time { return now }
	old := domain.Backup{ID: uuid.New(), AddonID: id, ProjectID: project, Status: domain.BackupRunning,
		CreatedAt: now.Add(-2 * time.Hour)}
	fresh := domain.Backup{ID: uuid.New(), AddonID: id, ProjectID: project, Status: domain.BackupRunning,
		CreatedAt: now.Add(-time.Minute)}
	require.NoError(t, store.SaveBackup(context.Background(), &old))
	require.NoError(t, store.SaveBackup(context.Background(), &fresh))

	list, err := s.ListBackups(context.Background(), project, id)
	require.NoError(t, err)
	byID := map[uuid.UUID]domain.Backup{}
	for _, b := range list.Backups {
		byID[b.ID] = b
	}
	assert.Equal(t, domain.BackupFailed, byID[old.ID].Status)
	assert.Equal(t, domain.ErrBackupInterrupted.Error(), byID[old.ID].Error)
	assert.Equal(t, domain.BackupRunning, byID[fresh.ID].Status, "une copie récente est vraiment en cours")
}
