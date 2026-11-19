package databases

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Contenu d'une base (mesuré à la demande), inspection d'un dump avant
// import, et remise à zéro. Avant de vider une base, une copie (pg_dump) est
// rangée dans le stockage d'objets et se restaure pendant 24 h : aucune
// sauvegarde n'existe sinon pour revenir sur une erreur de base.

// SnapshotStore : copies de sécurité enregistrées.
type SnapshotStore interface {
	GetSnapshot(ctx context.Context, databaseID uuid.UUID) (*domain.Snapshot, error)
	SaveSnapshot(ctx context.Context, s *domain.Snapshot) error
	DeleteSnapshot(ctx context.Context, databaseID uuid.UUID) error
}

func snapshotKey(databaseID uuid.UUID, taskID string) string {
	return "database-snapshots/" + databaseID.String() + "/" + taskID + ".pgc"
}

// snapshotURLValidity : durée de l'URL d'envoi de la copie (Job de remise à zéro).
const snapshotURLValidity = 30 * time.Minute

// pendingUploads : copies attendues (tâche → objet) ; l'URL signée ne sert
// que pendant la tâche, une seule fois.
var pendingUploads sync.Map

type pendingUpload struct {
	key  string
	mu   sync.Mutex
	size int64
	done bool
}

// dbLocks : une seule remise à zéro ou restauration à la fois par base
// (un double clic prendrait sinon la copie d'une base déjà vide).
var dbLocks sync.Map

func lockDatabase(id uuid.UUID) func() {
	m, _ := dbLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) shellReady() error {
	if err := s.available(); err != nil {
		return err
	}
	if s.Shell == nil {
		return domain.ErrShellDisabled
	}
	return nil
}

// target : une base prête, vue par les Jobs (espace, Cluster, image).
type target struct {
	db *domain.Database
	ns string
	TaskTarget
}

// readyDatabase : la base, son espace de noms, et une instance prête.
func (s *Service) readyDatabase(ctx context.Context, projectID, id uuid.UUID) (*target, error) {
	d, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.namespace(ctx, projectID)
	if err != nil {
		return nil, err
	}
	st, err := s.Clusters.State(ctx, ns, d.ClusterName())
	if err != nil {
		return nil, err
	}
	if !st.ReadyInstance || st.Image == "" {
		return nil, domain.ErrNotReady
	}
	return &target{db: d, ns: ns, TaskTarget: TaskTarget{Cluster: d.ClusterName(), Image: st.Image}}, nil
}

// Contents : taille et tables par schéma, lues dans la base.
func (s *Service) Contents(ctx context.Context, projectID, id uuid.UUID) (*domain.Contents, error) {
	if err := s.shellReady(); err != nil {
		return nil, err
	}
	t, err := s.readyDatabase(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	return s.contents(ctx, t)
}

func (s *Service) contents(ctx context.Context, t *target) (*domain.Contents, error) {
	c, err := s.Shell.Contents(ctx, t.ns, t.TaskTarget)
	if err != nil {
		return nil, fmt.Errorf("database contents: %w", err)
	}
	if c.Schemas == nil {
		c.Schemas = []domain.SchemaTables{}
	}
	c.MeasuredAt = s.now()
	return &c, nil
}

// Inspect : schémas et tables du dump (son début suffit), comparés à la base.
func (s *Service) Inspect(ctx context.Context, projectID, id uuid.UUID, head io.Reader) (*domain.InspectResult, error) {
	if err := s.shellReady(); err != nil {
		return nil, err
	}
	dump, err := domain.ReadDumpTOC(io.LimitReader(head, domain.InspectHeadBytes))
	if err != nil {
		return nil, err
	}
	t, err := s.readyDatabase(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	c, err := s.contents(ctx, t)
	if err != nil {
		return nil, err
	}
	out := domain.Compare(dump, *c)
	return &out, nil
}

// Reset : copie de sécurité, puis la base est vidée de tout ce qui
// appartient à l'utilisateur « app ». Le nom retapé confirme.
func (s *Service) Reset(ctx context.Context, projectID, id uuid.UUID, confirm, actor string) (*domain.ResetResponse, error) {
	if err := s.resetReady(); err != nil {
		return nil, err
	}
	t, err := s.readyDatabase(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(confirm) != t.db.Name {
		return nil, domain.ErrConfirmMismatch
	}
	if running, err := s.runningImport(ctx, t.ns, t.db.ID); err != nil {
		return nil, err
	} else if running {
		return nil, domain.ErrImportRunning
	}
	snap, err := s.reset(ctx, t, actor)
	if err != nil {
		return nil, err
	}
	return &domain.ResetResponse{Snapshot: *snap}, nil
}

func (s *Service) resetReady() error {
	if err := s.shellReady(); err != nil {
		return err
	}
	// Pas de copie possible sans stockage d'objets : on ne vide pas.
	if !s.Imports.ready() || s.Snapshots == nil {
		return domain.ErrShellDisabled
	}
	return nil
}

// reset : la copie, puis la remise à zéro (appelé aussi par l'import avec
// « vider d'abord »), dans un même Job : si la copie n'arrive pas à l'API,
// rien n'est vidé. Une base déjà vide garde la copie existante : une seconde
// remise à zéro ne doit pas remplacer la bonne copie par du vide.
func (s *Service) reset(ctx context.Context, t *target, actor string) (*domain.Snapshot, error) {
	defer lockDatabase(t.db.ID)()
	c, err := s.contents(ctx, t)
	if err != nil {
		return nil, err
	}
	prev, err := s.Snapshots.GetSnapshot(ctx, t.db.ID)
	if err != nil {
		return nil, err
	}
	if c.Tables == 0 && prev != nil && !prev.Expired(s.now()) {
		if err := s.Shell.Reset(ctx, t.ns, t.TaskTarget, nil); err != nil {
			return nil, fmt.Errorf("the database could not be emptied: %w", err)
		}
		return prev, nil
	}

	taskID := uuid.New()
	key := snapshotKey(t.db.ID, taskID.String())
	up := &pendingUpload{key: key}
	pendingUploads.Store(taskID, up)
	defer pendingUploads.Delete(taskID)
	exp := s.now().Add(snapshotURLValidity).Unix()
	upload := &SnapshotUpload{
		URL: fmt.Sprintf("%s/databases/snapshots/%s/upload?expires=%d&sig=%s",
			strings.TrimRight(s.Imports.PublicAPIURL, "/"), taskID, exp, s.Imports.Signer.SnapshotSignature(taskID, exp)),
		FetchImage: s.Imports.FetchImage, EgressAllow: s.Imports.EgressAllow,
	}
	if err := s.Shell.Reset(ctx, t.ns, t.TaskTarget, upload); err != nil {
		s.deleteObject(context.WithoutCancel(ctx), key)
		return nil, fmt.Errorf("the database was not emptied: %w", err)
	}
	up.mu.Lock()
	size, received := up.size, up.done
	up.mu.Unlock()
	if !received {
		// Ne devrait pas arriver : le Job n'a pas d'issue sans l'envoi.
		return nil, errors.New("the safety copy did not reach the API")
	}
	now := s.now()
	snap := &domain.Snapshot{DatabaseID: t.db.ID, ObjectKey: key, SizeBytes: size, TakenBy: actor, TakenAt: now, ExpiresAt: now.Add(domain.SnapshotRetention)}
	if err := s.Snapshots.SaveSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	if prev != nil && prev.ObjectKey != key {
		s.deleteObject(ctx, prev.ObjectKey)
	}
	return snap, nil
}

// ReceiveSnapshot : la copie envoyée par le Job (URL signée, sans session),
// acceptée une fois et seulement pendant sa tâche.
func (s *Service) ReceiveSnapshot(ctx context.Context, taskID uuid.UUID, expires int64, sig string, r io.Reader, size int64) error {
	if !s.Imports.ready() || !s.Imports.Signer.VerifySnapshot(taskID, expires, sig, s.now()) {
		return domain.ErrNoSnapshot
	}
	v, ok := pendingUploads.Load(taskID)
	if !ok {
		return domain.ErrNoSnapshot
	}
	up := v.(*pendingUpload)
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.done {
		return domain.ErrNoSnapshot
	}
	if size > s.MaxDumpBytes() {
		return domain.ErrDumpTooLarge
	}
	counter := &countingReader{r: io.LimitReader(r, s.MaxDumpBytes()+1)}
	if err := s.Imports.Dumps.Put(ctx, up.key, counter, size, "application/octet-stream"); err != nil {
		return err
	}
	if counter.n > s.MaxDumpBytes() || counter.n == 0 {
		s.deleteObject(ctx, up.key)
		return domain.ErrDumpTooLarge
	}
	up.size, up.done = counter.n, true
	return nil
}

func (s *Service) deleteObject(ctx context.Context, key string) {
	if !s.Imports.ready() || key == "" {
		return
	}
	if err := s.Imports.Dumps.Delete(ctx, key); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		s.warn("safety copy not deleted", "key", key, "error", err)
	}
}

// UndoReset : la copie de sécurité est réimportée (un import ordinaire,
// objets existants remplacés), tant qu'elle n'a pas expiré.
func (s *Service) UndoReset(ctx context.Context, projectID, id uuid.UUID, actor string) (*domain.DatabaseImport, error) {
	if err := s.resetReady(); err != nil {
		return nil, err
	}
	d, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	unlock := lockDatabase(d.ID)
	defer unlock()
	snap, err := s.Snapshots.GetSnapshot(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if snap == nil || snap.Expired(s.now()) {
		return nil, domain.ErrNoSnapshot
	}
	obj, info, err := s.Imports.Dumps.Get(ctx, snap.ObjectKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, domain.ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return s.Import(ctx, projectID, d.ID, obj, info.Size, true, false, actor)
}

// snapshotView : copie restaurable de la base ; une copie expirée est
// effacée au passage (au mieux).
func (s *Service) snapshotView(ctx context.Context, id uuid.UUID) *domain.Snapshot {
	if s.Snapshots == nil {
		return nil
	}
	snap, err := s.Snapshots.GetSnapshot(ctx, id)
	if err != nil || snap == nil {
		return nil
	}
	if snap.Expired(s.now()) {
		s.dropSnapshot(context.WithoutCancel(ctx), id)
		return nil
	}
	return snap
}

func (s *Service) dropSnapshot(ctx context.Context, id uuid.UUID) {
	if s.Snapshots == nil {
		return
	}
	snap, err := s.Snapshots.GetSnapshot(ctx, id)
	if err != nil || snap == nil {
		return
	}
	s.deleteObject(ctx, snap.ObjectKey)
	if err := s.Snapshots.DeleteSnapshot(ctx, id); err != nil {
		s.warn("safety copy row not deleted", "database", id, "error", err)
	}
}

func (s *Service) now() time.Time {
	if s.Imports != nil {
		return s.Imports.now()
	}
	return time.Now()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// ── Stockage ────────────────────────────────────────────────────────────

func (r *GormRepository) GetSnapshot(ctx context.Context, databaseID uuid.UUID) (*domain.Snapshot, error) {
	var snap domain.Snapshot
	err := r.DB.WithContext(ctx).Where("database_id = ?", databaseID).First(&snap).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func (r *GormRepository) SaveSnapshot(ctx context.Context, snap *domain.Snapshot) error {
	return r.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(snap).Error
}

func (r *GormRepository) DeleteSnapshot(ctx context.Context, databaseID uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("database_id = ?", databaseID).Delete(&domain.Snapshot{}).Error
}
