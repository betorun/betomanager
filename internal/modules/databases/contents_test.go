package databases

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// fakeShell : une base en mémoire (tables) ; la remise à zéro joue le Job :
// copie envoyée à l'API par l'URL signée, puis la base vidée.
type fakeShell struct {
	mu      sync.Mutex
	svc     *Service
	tables  int
	dumpErr error
	// emptyErr : la copie part, puis le vidage échoue.
	emptyErr error
	dumps    int
	resets   int
}

func (f *fakeShell) Contents(context.Context, string, TaskTarget) (domain.Contents, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sc := []domain.SchemaTables{}
	if f.tables > 0 {
		sc = append(sc, domain.SchemaTables{Name: "bomba", Tables: f.tables})
	}
	return domain.Contents{SizeBytes: 8 << 20, Tables: f.tables, Schemas: sc}, nil
}

func (f *fakeShell) Reset(ctx context.Context, _ string, _ TaskTarget, snap *SnapshotUpload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if snap != nil {
		if f.dumpErr != nil {
			return f.dumpErr
		}
		u, err := url.Parse(snap.URL)
		if err != nil {
			return err
		}
		parts := strings.Split(u.Path, "/")
		id := uuid.MustParse(parts[len(parts)-2])
		exp, _ := strconv.ParseInt(u.Query().Get("expires"), 10, 64)
		data := append([]byte("PGDMP"), bytes.Repeat([]byte{byte(f.tables)}, 10)...)
		if err := f.svc.ReceiveSnapshot(ctx, id, exp, u.Query().Get("sig"), bytes.NewReader(data), int64(len(data))); err != nil {
			return err
		}
		f.dumps++
		// Rejouer l'envoi : refusé (une seule fois par tâche).
		if err := f.svc.ReceiveSnapshot(ctx, id, exp, u.Query().Get("sig"), bytes.NewReader(data), int64(len(data))); err == nil {
			return errors.New("second envoi accepté")
		}
	}
	if f.emptyErr != nil {
		return f.emptyErr
	}
	f.resets++
	f.tables = 0
	return nil
}

// Dump : la copie seule (rien n'est vidé).
func (f *fakeShell) Dump(ctx context.Context, _ string, _ TaskTarget, up *SnapshotUpload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dumpErr != nil {
		return f.dumpErr
	}
	u, err := url.Parse(up.URL)
	if err != nil {
		return err
	}
	parts := strings.Split(u.Path, "/")
	id := uuid.MustParse(parts[len(parts)-2])
	exp, _ := strconv.ParseInt(u.Query().Get("expires"), 10, 64)
	data := append([]byte("PGDMP"), bytes.Repeat([]byte{byte(f.tables)}, 10)...)
	if err := f.svc.ReceiveSnapshot(ctx, id, exp, u.Query().Get("sig"), bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	f.dumps++
	return nil
}

type memSnapshots struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.Snapshot
	// failSave : numéros des enregistrements (1, 2…) qui échouent.
	failSave map[int]bool
	saves    int
}

func (m *memSnapshots) GetSnapshot(_ context.Context, id uuid.UUID) (*domain.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[id]; ok {
		return &s, nil
	}
	return nil, nil
}
func (m *memSnapshots) SaveSnapshot(_ context.Context, s *domain.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.failSave[m.saves] {
		return errors.New("column \"object_key\" does not exist")
	}
	m.rows[s.DatabaseID] = *s
	return nil
}
func (m *memSnapshots) MarkSnapshotRestore(_ context.Context, id, importID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[id]; ok {
		s.RestoreImportID = &importID
		m.rows[id] = s
	}
	return nil
}
func (m *memSnapshots) DeleteSnapshot(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func resetSvc(t *testing.T, tables int) (*Service, *domain.Database, *fakeShell, *memSnapshots, *memObjects, *fakeImporter) {
	t.Helper()
	s, d, imp, _, objs := importSvc(t)
	sh := &fakeShell{tables: tables, svc: s}
	snaps := &memSnapshots{rows: map[uuid.UUID]domain.Snapshot{}}
	s.Shell, s.Snapshots = sh, snaps
	return s, d, sh, snaps, objs, imp
}

// La copie est prise et rangée AVANT que la base soit vidée.
func TestReset_SnapshotThenEmpty(t *testing.T) {
	ctx := context.Background()
	s, d, sh, snaps, objs, _ := resetSvc(t, 24)

	got, err := s.Reset(ctx, d.ProjectID, d.ID, " main ", "user:1")

	require.NoError(t, err)
	assert.Equal(t, 1, sh.dumps)
	assert.Equal(t, 1, sh.resets)
	assert.Equal(t, int64(15), got.Snapshot.SizeBytes)
	assert.Equal(t, importNow.Add(24*time.Hour), got.Snapshot.ExpiresAt)
	require.Contains(t, snaps.rows, d.ID)
	assert.Len(t, objs.data[snaps.rows[d.ID].ObjectKey], 15)
}

func TestReset_WrongNameChangesNothing(t *testing.T) {
	s, d, sh, _, _, _ := resetSvc(t, 24)
	_, err := s.Reset(context.Background(), d.ProjectID, d.ID, "autre", "u")
	assert.ErrorIs(t, err, domain.ErrConfirmMismatch)
	assert.Zero(t, sh.dumps+sh.resets)
}

// Copie impossible : la base n'est pas vidée.
func TestReset_DumpFailureKeepsData(t *testing.T) {
	s, d, sh, snaps, _, _ := resetSvc(t, 24)
	sh.dumpErr = errors.New("pg_dump: boom")
	_, err := s.Reset(context.Background(), d.ProjectID, d.ID, "main", "u")
	require.Error(t, err)
	assert.Zero(t, sh.resets)
	assert.Equal(t, 24, sh.tables)
	assert.Empty(t, snaps.rows)
}

// Seconde remise à zéro d'une base déjà vide : la bonne copie est gardée.
func TestReset_EmptyDatabaseKeepsPreviousCopy(t *testing.T) {
	ctx := context.Background()
	s, d, sh, snaps, objs, _ := resetSvc(t, 24)
	_, err := s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.NoError(t, err)
	key := snaps.rows[d.ID].ObjectKey

	_, err = s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.NoError(t, err)
	assert.Equal(t, 1, sh.dumps, "pas de nouvelle copie d'une base vide")
	assert.Equal(t, 2, sh.resets)
	assert.Equal(t, key, snaps.rows[d.ID].ObjectKey)
	assert.Len(t, objs.data[key], 15)
}

// La copie se réimporte (import ordinaire, objets remplacés) ; expirée, non.
func TestUndoReset(t *testing.T) {
	ctx := context.Background()
	s, d, _, snaps, _, imp := resetSvc(t, 24)
	_, err := s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.NoError(t, err)

	got, err := s.UndoReset(ctx, d.ProjectID, d.ID, "u")
	require.NoError(t, err)
	assert.True(t, got.Clean)
	require.Len(t, imp.started, 1)

	// La copie est liée à l'import : « déjà restaurée » une fois qu'il a réussi.
	require.Equal(t, got.ID, *snaps.rows[d.ID].RestoreImportID)
	assert.Nil(t, s.snapshotView(ctx, d.ID).RestoredAt, "import still running")
	store := s.Imports.Store.(*memImports)
	done := importNow.Add(time.Minute)
	store.rows[got.ID].Status, store.rows[got.ID].FinishedAt = domain.ImportSucceeded, &done
	require.NotNil(t, s.snapshotView(ctx, d.ID).RestoredAt)
	assert.Equal(t, done, *s.snapshotView(ctx, d.ID).RestoredAt)

	snap := snaps.rows[d.ID]
	snap.ExpiresAt = importNow
	snaps.rows[d.ID] = snap
	imp.state = ImportJobState{Exists: true, Done: true}
	_, err = s.UndoReset(ctx, d.ProjectID, d.ID, "u")
	assert.ErrorIs(t, err, domain.ErrNoSnapshot)
}

// Import avec « vider d'abord » : copie, remise à zéro, puis le Job.
func TestImport_ResetFirst(t *testing.T) {
	s, d, sh, _, _, imp := resetSvc(t, 24)
	_, err := s.Import(context.Background(), d.ProjectID, d.ID, bytes.NewReader(dump), int64(len(dump)), false, true, "u")
	require.NoError(t, err)
	assert.Equal(t, 1, sh.dumps)
	assert.Equal(t, 1, sh.resets)
	assert.Len(t, imp.started, 1)
}

// Sans stockage pour la copie, on ne vide jamais.
func TestReset_RefusedWithoutSnapshotStorage(t *testing.T) {
	s, d, sh, _, _, _ := resetSvc(t, 24)
	s.Snapshots = nil
	_, err := s.Reset(context.Background(), d.ProjectID, d.ID, "main", "u")
	assert.ErrorIs(t, err, domain.ErrShellDisabled)
	assert.Zero(t, sh.resets)
}

func TestCompare(t *testing.T) {
	dump := domain.DumpSummary{Schemas: []domain.SchemaTables{{Name: "bomba", Tables: 33}}, Tables: 33}
	empty := domain.Contents{Schemas: []domain.SchemaTables{{Name: "public"}}}
	same := domain.Contents{Tables: 24, Schemas: []domain.SchemaTables{{Name: "bomba", Tables: 24}, {Name: "public"}}}
	other := domain.Contents{Tables: 90, Schemas: []domain.SchemaTables{{Name: "keycloak", Tables: 90}}}

	got := domain.Compare(dump, empty)
	assert.Empty(t, got.Conflicts)
	assert.False(t, got.Foreign)

	got = domain.Compare(dump, same)
	assert.Equal(t, []string{"bomba"}, got.Conflicts)
	assert.False(t, got.Foreign)

	got = domain.Compare(dump, other)
	assert.Empty(t, got.Conflicts)
	assert.True(t, got.Foreign, "aucun schéma commun : sans doute la mauvaise base")
}

// URL d'envoi : signature fausse ou tâche inconnue, refusé.
func TestReceiveSnapshot_Refused(t *testing.T) {
	s, _, _, _, _, _ := resetSvc(t, 1)
	id := uuid.New()
	exp := importNow.Add(time.Minute).Unix()
	err := s.ReceiveSnapshot(context.Background(), id, exp, s.Imports.Signer.SnapshotSignature(id, exp), bytes.NewReader([]byte("x")), 1)
	assert.ErrorIs(t, err, domain.ErrNoSnapshot, "tâche inconnue")
	pendingUploads.Store(id, &pendingUpload{key: "k"})
	defer pendingUploads.Delete(id)
	err = s.ReceiveSnapshot(context.Background(), id, exp, "faux", bytes.NewReader([]byte("x")), 1)
	assert.ErrorIs(t, err, domain.ErrNoSnapshot, "signature fausse")
}

// Job de remise à zéro : copie puis envoi en amorces, la base n'est vidée
// qu'après ; connecté en « app » (secret du Cluster), politique des imports.
func TestResetJobSpec(t *testing.T) {
	tg := TaskTarget{Cluster: "db-main", Image: "pg:17"}
	job := taskJob("km-p", "db-reset-1", "1", tg, []corev1.Container{
		{Name: "dump", Image: "pg:17"}, {Name: "upload", Image: "curl:1"},
	}, corev1.Container{Name: "main", Image: "pg:17"}, resetTimeout)
	spec := job.Spec.Template.Spec
	require.Len(t, spec.InitContainers, 2)
	assert.Equal(t, "dump", spec.InitContainers[0].Name)
	assert.Equal(t, "upload", spec.InitContainers[1].Name)
	assert.Empty(t, spec.InitContainers[1].Env, "l'envoi ne reçoit pas le mot de passe")
	assert.Equal(t, importRole, job.Spec.Template.Labels[importRoleLabel])
	var pw *corev1.EnvVar
	for i, e := range spec.Containers[0].Env {
		if e.Name == "PGPASSWORD" {
			pw = &spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, pw)
	assert.Equal(t, "db-main-app", pw.ValueFrom.SecretKeyRef.Name)
	assert.False(t, *spec.AutomountServiceAccountToken)
}

// Incident du 19 nov. : l'enregistrement de la copie échouait APRÈS le
// vidage. Désormais la ligne est écrite avant : si elle ne peut pas l'être,
// rien n'est vidé.
func TestReset_RecordFailsBeforeEmptying(t *testing.T) {
	s, d, sh, snaps, _, _ := resetSvc(t, 24)
	snaps.failSave = map[int]bool{1: true}
	_, err := s.Reset(context.Background(), d.ProjectID, d.ID, "main", "u")
	require.Error(t, err)
	assert.Zero(t, sh.resets, "rien n'est vidé")
	assert.Equal(t, 24, sh.tables)
}

// Après le vidage, un échec d'écriture ne fait plus perdre la copie.
func TestReset_CopyRestorableEvenIfLaterWriteFails(t *testing.T) {
	ctx := context.Background()
	s, d, sh, snaps, objs, _ := resetSvc(t, 24)
	snaps.failSave = map[int]bool{2: true} // mise à jour de la taille
	_, err := s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.NoError(t, err)
	assert.Equal(t, 1, sh.resets)
	require.Contains(t, snaps.rows, d.ID, "la copie est enregistrée")
	assert.Len(t, objs.data[snaps.rows[d.ID].ObjectKey], 15)
	_, err = s.UndoReset(ctx, d.ProjectID, d.ID, "u")
	assert.NoError(t, err)
}

// Copie arrivée, vidage en échec : la copie est gardée et l'erreur le dit.
func TestReset_EmptyFailsKeepsCopy(t *testing.T) {
	s, d, sh, snaps, objs, _ := resetSvc(t, 24)
	sh.emptyErr = errors.New("lock timeout")
	_, err := s.Reset(context.Background(), d.ProjectID, d.ID, "main", "u")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "safety copy is kept")
	require.Contains(t, snaps.rows, d.ID)
	assert.Equal(t, int64(15), snaps.rows[d.ID].SizeBytes)
	assert.Len(t, objs.data[snaps.rows[d.ID].ObjectKey], 15)
}

// Copie jamais arrivée : la copie précédente est remise, rien n'est vidé.
func TestReset_NoUploadRestoresPreviousCopy(t *testing.T) {
	ctx := context.Background()
	s, d, sh, snaps, objs, _ := resetSvc(t, 24)
	_, err := s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.NoError(t, err)
	prev := snaps.rows[d.ID]

	sh.tables = 5
	sh.dumpErr = errors.New("pg_dump: boom")
	_, err = s.Reset(ctx, d.ProjectID, d.ID, "main", "u")
	require.Error(t, err)
	assert.Equal(t, 5, sh.tables, "rien n'est vidé")
	assert.Equal(t, prev, snaps.rows[d.ID])
	assert.Len(t, objs.data[prev.ObjectKey], 15)
}
