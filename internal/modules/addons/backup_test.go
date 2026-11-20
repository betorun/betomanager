package addons

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
)

type memBackups struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.Backup
}

func (m *memBackups) CreateBackup(_ context.Context, b *domain.Backup) error {
	return m.SaveBackup(context.Background(), b)
}
func (m *memBackups) SaveBackup(_ context.Context, b *domain.Backup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[b.ID] = *b
	return nil
}
func (m *memBackups) ListBackups(_ context.Context, addon uuid.UUID) ([]domain.Backup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Backup
	for _, b := range m.rows {
		if b.AddonID == addon {
			out = append(out, b)
		}
	}
	// Plus récentes d'abord.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}
func (m *memBackups) GetBackup(_ context.Context, addon, id uuid.UUID) (*domain.Backup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.rows[id]
	if !ok || b.AddonID != addon {
		return nil, domain.ErrBackupNotFound
	}
	return &b, nil
}
func (m *memBackups) DeleteBackup(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}
func (m *memBackups) DueBackups(context.Context, []string, time.Time) ([]domain.Addon, error) {
	return nil, nil
}

type memObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (o *memObjects) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data[key] = b
	return err
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func (o *memObjects) Get(_ context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.data[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return readSeekCloser{bytes.NewReader(b)}, objectstore.ObjectInfo{Size: int64(len(b))}, nil
}
func (o *memObjects) Download(context.Context, string, io.Writer) error { return nil }
func (o *memObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.data, key)
	return nil
}
func (o *memObjects) DeletePrefix(_ context.Context, prefix string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k := range o.data {
		if strings.HasPrefix(k, prefix) {
			delete(o.data, k)
		}
	}
	return nil
}

// fakeRunner : joue le Job en appelant l'API par l'URL signée reçue.
type fakeRunner struct {
	s        *Service
	payload  string
	failDump bool
	tasks    []BackupTask
	restored []string
}

func transferArgs(t *testing.T, raw string) (uuid.UUID, int64, string) {
	u, err := url.Parse(raw)
	require.NoError(t, err)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	id := uuid.MustParse(parts[len(parts)-2])
	exp, _ := strconv.ParseInt(u.Query().Get("expires"), 10, 64)
	return id, exp, u.Query().Get("sig")
}

var runnerT *testing.T

func (f *fakeRunner) Dump(ctx context.Context, t BackupTask) error {
	f.tasks = append(f.tasks, t)
	if f.failDump {
		return errors.New("the backup task failed: service: Access denied")
	}
	id, exp, sig := transferArgs(runnerT, t.URL)
	return f.s.ReceiveBackup(ctx, id, exp, sig, strings.NewReader(f.payload), int64(len(f.payload)))
}

func (f *fakeRunner) Restore(ctx context.Context, t BackupTask) error {
	f.tasks = append(f.tasks, t)
	id, exp, sig := transferArgs(runnerT, t.URL)
	obj, _, err := f.s.OpenBackup(ctx, id, exp, sig)
	if err != nil {
		return err
	}
	defer obj.Close()
	b, _ := io.ReadAll(obj)
	f.restored = append(f.restored, string(b))
	return nil
}

func backupService(t *testing.T) (*Service, *fakeRunner, *memBackups, *memObjects, uuid.UUID, uuid.UUID) {
	t.Helper()
	runnerT = t
	s, _, wl, project := newTestService(t, 0)
	wl.running = true
	store := &memBackups{rows: map[uuid.UUID]domain.Backup{}}
	objs := &memObjects{data: map[string][]byte{}}
	runner := &fakeRunner{s: s, payload: "dump-1"}
	s.Backups = &BackupConfig{Store: store, Objects: objs, Signer: registry.Credentials{Secret: []byte("test-secret")},
		Runner: runner, PublicAPIURL: "https://api.test/kmanager/v1", FetchImage: "curl"}
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "mariadb", Name: "shop-db"}, "")
	require.NoError(t, err)
	return s, runner, store, objs, project, uuid.MustParse(v.ID)
}

func waitBackup(t *testing.T, store *memBackups, id uuid.UUID) domain.Backup {
	t.Helper()
	for i := 0; i < 200; i++ {
		store.mu.Lock()
		b := store.rows[id]
		store.mu.Unlock()
		if b.Status != domain.BackupRunning {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sauvegarde toujours en cours")
	return domain.Backup{}
}

func TestBackup_ManualStoresCopyAndSignsURL(t *testing.T) {
	s, runner, store, objs, project, id := backupService(t)
	b, err := s.StartBackup(context.Background(), project, id, "user:1")
	require.NoError(t, err)
	assert.Equal(t, domain.BackupRunning, b.Status)
	done := waitBackup(t, store, b.ID)
	assert.Equal(t, domain.BackupCompleted, done.Status)
	assert.Equal(t, int64(6), done.SizeBytes)
	assert.Equal(t, "dump-1", string(objs.data[done.ObjectKey]))

	task := runner.tasks[0]
	assert.Equal(t, "shop-db", task.Host)
	assert.Equal(t, "mariadb:11.8.9", task.Image)
	assert.Equal(t, "MYSQL_PWD", task.Commands.PasswordEnv)
	assert.NotContains(t, task.URL, "ADDON_PASSWORD")
	// Une URL d'envoi ne sert qu'une fois, et pas à lire.
	tid, exp, sig := transferArgs(t, task.URL)
	assert.ErrorIs(t, s.ReceiveBackup(context.Background(), tid, exp, sig, strings.NewReader("x"), 1), domain.ErrBackupNotFound)
	_, _, err = s.OpenBackup(context.Background(), tid, exp, sig)
	assert.ErrorIs(t, err, domain.ErrBackupNotFound)

	list, err := s.ListBackups(context.Background(), project, id)
	require.NoError(t, err)
	assert.Len(t, list.Backups, 1)
	assert.Equal(t, domain.BackupsKept, list.Kept)
}

func TestBackup_FailureIsRecordedWithoutObject(t *testing.T) {
	s, runner, store, objs, project, id := backupService(t)
	runner.failDump = true
	b, err := s.StartBackup(context.Background(), project, id, "")
	require.NoError(t, err)
	done := waitBackup(t, store, b.ID)
	assert.Equal(t, domain.BackupFailed, done.Status)
	assert.Contains(t, done.Error, "Access denied")
	assert.Empty(t, objs.data)
}

func TestBackup_RefusedForServicesWithoutBackups(t *testing.T) {
	s, _, _, _, project, _ := backupService(t)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache"}, "")
	require.NoError(t, err)
	_, err = s.StartBackup(context.Background(), project, uuid.MustParse(v.ID), "")
	assert.ErrorIs(t, err, domain.ErrNoBackups)
	_, err = s.ListBackups(context.Background(), project, uuid.MustParse(v.ID))
	assert.ErrorIs(t, err, domain.ErrNoBackups)
}

func TestBackup_RestoreTakesSafetyCopyFirst(t *testing.T) {
	s, runner, store, objs, project, id := backupService(t)
	b, err := s.StartBackup(context.Background(), project, id, "")
	require.NoError(t, err)
	waitBackup(t, store, b.ID)

	_, err = s.Restore(context.Background(), project, id, b.ID, "autre", "user:1")
	assert.ErrorIs(t, err, domain.ErrConfirmMismatch)

	runner.payload = "dump-now"
	got, err := s.Restore(context.Background(), project, id, b.ID, "shop-db", "user:1")
	require.NoError(t, err)
	assert.NotNil(t, got.RestoredAt)
	assert.Equal(t, []string{"dump-1"}, runner.restored, "la sauvegarde choisie est rechargée")
	list, _ := s.ListBackups(context.Background(), project, id)
	require.Len(t, list.Backups, 2)
	assert.Equal(t, domain.TriggerBeforeRestore, list.Backups[0].Trigger)
	assert.Equal(t, "dump-now", string(objs.data[list.Backups[0].ObjectKey]), "copie de l'état d'avant la restauration")

	// Copie de sécurité impossible : rien n'est rechargé.
	runner.failDump = true
	_, err = s.Restore(context.Background(), project, id, b.ID, "shop-db", "")
	assert.Error(t, err)
	assert.Len(t, runner.restored, 1)
}

func TestBackup_PruneKeepsLastCompleted(t *testing.T) {
	s, _, store, objs, _, id := backupService(t)
	now := time.Now()
	for i := 0; i < domain.BackupsKept+3; i++ {
		bid := uuid.New()
		key := backupKey(id, bid)
		objs.data[key] = []byte("x")
		store.rows[bid] = domain.Backup{ID: bid, AddonID: id, Status: domain.BackupCompleted, ObjectKey: key, CreatedAt: now.Add(-time.Duration(i) * time.Hour)}
	}
	old := uuid.New()
	store.rows[old] = domain.Backup{ID: old, AddonID: id, Status: domain.BackupFailed, CreatedAt: now.Add(-8 * 24 * time.Hour)}
	s.pruneBackups(context.Background(), id)
	assert.Len(t, store.rows, domain.BackupsKept)
	assert.Len(t, objs.data, domain.BackupsKept)
}

func TestBackup_DeleteServiceDropsCopies(t *testing.T) {
	s, _, store, objs, project, id := backupService(t)
	b, _ := s.StartBackup(context.Background(), project, id, "")
	waitBackup(t, store, b.ID)
	require.NotEmpty(t, objs.data)
	_, err := s.Delete(context.Background(), project, id)
	require.NoError(t, err)
	assert.Empty(t, objs.data)
}

func TestBackupJob_SecretsStayOutOfArguments(t *testing.T) {
	tpl := domain.MariaDB
	job := backupJob(BackupTask{TaskID: "t1", Namespace: "km-x", Host: "shop-db", Release: "shop-db", Image: "mariadb:11.8.9",
		RunAsUser: 999, Commands: *tpl.Backup, URL: "https://api/x?sig=s", FetchImage: "curl"}, "p1", false)
	pod := job.Spec.Template.Spec
	assert.Equal(t, "p1", job.Spec.Template.Labels["km-project-id"], "accepté par le réseau du projet")
	require.Len(t, pod.InitContainers, 1)
	dump := pod.InitContainers[0]
	assert.Equal(t, "mariadb:11.8.9", dump.Image)
	var pwd bool
	for _, e := range dump.Env {
		if e.Name == "MYSQL_PWD" {
			pwd = e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "shop-db-env" && e.ValueFrom.SecretKeyRef.Key == "ADDON_PASSWORD"
		}
	}
	assert.True(t, pwd, "mot de passe lu dans le secret du service")
	assert.Equal(t, "upload", pod.Containers[0].Name)
	assert.Equal(t, int64(999), *pod.SecurityContext.RunAsUser)

	restore := backupJob(BackupTask{TaskID: "t2", Release: "shop-db", Commands: *tpl.Backup, Image: "mariadb:11.8.9", FetchImage: "curl"}, "p1", true)
	assert.Equal(t, "fetch", restore.Spec.Template.Spec.InitContainers[0].Name)
	assert.Contains(t, restore.Spec.Template.Spec.Containers[0].Command[2], "gunzip")
}
