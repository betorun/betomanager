package addons

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

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Sauvegardes des services qui en ont (MariaDB). Un Job dans l'espace du
// projet lance la commande du modèle avec le mot de passe du service (lu
// dans son secret, jamais en argument) et envoie la copie à l'API par une
// URL signée ; l'API la range dans le stockage d'objets. La restauration
// fait l'inverse : copie de l'état courant d'abord, puis le Job lit la
// sauvegarde par une URL signée et la recharge.

// BackupEvery : fréquence des passages de la sauvegarde nocturne.
const BackupEvery = time.Hour

// Délais d'une tâche (démarrage du pod compris) et validité des URL.
const (
	backupTimeout     = 20 * time.Minute
	backupURLValidity = 30 * time.Minute
	// backupStale : une sauvegarde « en cours » plus vieille est un reste
	// (API relancée pendant la tâche) : elle ne bloque plus.
	backupStale = time.Hour
	// failedKept : les sauvegardes en échec restent visibles une semaine.
	failedKept = 7 * 24 * time.Hour
)

// BackupStore : sauvegardes enregistrées.
type BackupStore interface {
	CreateBackup(ctx context.Context, b *domain.Backup) error
	SaveBackup(ctx context.Context, b *domain.Backup) error
	ListBackups(ctx context.Context, addonID uuid.UUID) ([]domain.Backup, error)
	GetBackup(ctx context.Context, addonID, id uuid.UUID) (*domain.Backup, error)
	DeleteBackup(ctx context.Context, id uuid.UUID) error
	// DueBackups : services sauvegardables sans copie récente (nuit).
	DueBackups(ctx context.Context, templates []string, now time.Time) ([]domain.Addon, error)
}

// BackupSigner : URL signées des Jobs (registry.Credentials).
type BackupSigner interface {
	AddonBackupSignature(taskID uuid.UUID, direction string, expires int64) string
	VerifyAddonBackup(taskID uuid.UUID, direction string, expires int64, sig string, now time.Time) bool
}

// BackupTask : une copie ou une restauration à lancer dans le projet.
type BackupTask struct {
	TaskID    string
	ProjectID string
	Namespace string
	// Host : adresse du service ; Release : son conteneur (secret des variables).
	Host, Release string
	Port          int
	Image         string
	RunAsUser     int64
	Commands      domain.BackupCommands
	// URL : envoi (copie) ou lecture (restauration) de la sauvegarde.
	URL         string
	FetchImage  string
	EgressAllow []string
}

// BackupRunner : exécute les Jobs (Kubernetes ; nil : indisponible).
type BackupRunner interface {
	Dump(ctx context.Context, t BackupTask) error
	Restore(ctx context.Context, t BackupTask) error
}

// BackupConfig : ce qu'il faut pour sauvegarder (absent : indisponible).
type BackupConfig struct {
	Store   BackupStore
	Objects objectstore.Store
	Signer  BackupSigner
	Runner  BackupRunner
	// PublicAPIURL : API vue par les pods.
	PublicAPIURL string
	FetchImage   string
	EgressAllow  []string
	MaxBytes     int64
	Now          func() time.Time
}

// defaultMaxBackupBytes : taille maximale d'une copie compressée.
const defaultMaxBackupBytes = 20 << 30

func (c *BackupConfig) ready() bool {
	return c != nil && c.Store != nil && c.Objects != nil && c.Signer != nil && c.Runner != nil && c.PublicAPIURL != "" && c.FetchImage != ""
}

func (c *BackupConfig) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *BackupConfig) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return defaultMaxBackupBytes
}

func backupKey(addonID, backupID uuid.UUID) string {
	return "addon-backups/" + addonID.String() + "/" + backupID.String() + ".gz"
}

// Transferts attendus : tâche → objet. L'URL signée ne sert que pendant la
// tâche, et une seule fois pour un envoi.
var backupTransfers sync.Map

type backupTransfer struct {
	key    string
	upload bool
	mu     sync.Mutex
	size   int64
	done   bool
}

// addonLocks : une copie ou une restauration à la fois par service.
var addonLocks sync.Map

func tryLockAddon(id uuid.UUID) (func(), bool) {
	m, _ := addonLocks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// backupTarget : service sauvegardable, en marche, et son espace.
type backupTarget struct {
	addon    domain.Addon
	template domain.Template
	version  domain.Version
	ns       string
}

func (s *Service) backupTarget(ctx context.Context, projectID, id uuid.UUID, needRunning bool) (*backupTarget, error) {
	if !s.Backups.ready() || s.Workloads == nil {
		return nil, domain.ErrBackupsUnavailable
	}
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	t, ok := domain.TemplateByID(a.Template)
	if !ok || t.Backup == nil {
		return nil, domain.ErrNoBackups
	}
	v, _, ok := t.Version(a.Version)
	if !ok {
		v = t.Latest()
	}
	p, err := s.Projects.GetProjectByID(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	if needRunning {
		states, err := s.Workloads.States(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if states[a.WorkloadID].Status != utils.WorkloadRunning {
			return nil, domain.ErrNotReady
		}
	}
	return &backupTarget{addon: *a, template: t, version: v, ns: p.Namespace}, nil
}

func (s *Service) task(t *backupTarget, taskID uuid.UUID, direction string) BackupTask {
	exp := s.Backups.now().Add(backupURLValidity).Unix()
	action := "upload"
	if direction == "down" {
		action = "download"
	}
	return BackupTask{
		TaskID: taskID.String(), ProjectID: t.addon.ProjectID.String(), Namespace: t.ns, Host: t.addon.Name, Release: t.addon.Name, Port: t.template.Port,
		Image: t.version.Image, RunAsUser: t.template.RunAsUser, Commands: *t.template.Backup,
		URL: fmt.Sprintf("%s/addons/backups/%s/%s?expires=%d&sig=%s", strings.TrimRight(s.Backups.PublicAPIURL, "/"),
			taskID, action, exp, s.Backups.Signer.AddonBackupSignature(taskID, direction, exp)),
		FetchImage: s.Backups.FetchImage, EgressAllow: s.Backups.EgressAllow,
	}
}

// ListBackups : sauvegardes d'un service, les plus récentes d'abord.
func (s *Service) ListBackups(ctx context.Context, projectID, id uuid.UUID) (*domain.BackupList, error) {
	if !s.Backups.ready() {
		return nil, domain.ErrBackupsUnavailable
	}
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if t, ok := domain.TemplateByID(a.Template); !ok || t.Backup == nil {
		return nil, domain.ErrNoBackups
	}
	list, err := s.Backups.Store.ListBackups(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Backup{}
	}
	return &domain.BackupList{Backups: list, Kept: domain.BackupsKept}, nil
}

// StartBackup : copie demandée dans la console ; elle tourne en arrière-plan
// (la liste la montre « en cours »).
func (s *Service) StartBackup(ctx context.Context, projectID, id uuid.UUID, actor string) (*domain.Backup, error) {
	t, err := s.backupTarget(ctx, projectID, id, true)
	if err != nil {
		return nil, err
	}
	unlock, ok := tryLockAddon(t.addon.ID)
	if !ok {
		return nil, domain.ErrBackupRunning
	}
	b, err := s.newBackup(ctx, t, domain.TriggerManual, actor)
	if err != nil {
		unlock()
		return nil, err
	}
	go func() {
		defer unlock()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupTimeout+time.Minute)
		defer cancel()
		s.runBackup(bg, t, b)
	}()
	return b, nil
}

func (s *Service) newBackup(ctx context.Context, t *backupTarget, trigger, actor string) (*domain.Backup, error) {
	id := uuid.New()
	b := &domain.Backup{ID: id, AddonID: t.addon.ID, ProjectID: t.addon.ProjectID, Status: domain.BackupRunning,
		Trigger: trigger, ObjectKey: backupKey(t.addon.ID, id), CreatedBy: actor, CreatedAt: s.Backups.now()}
	if err := s.Backups.Store.CreateBackup(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// runBackup : le Job de copie, puis le résultat enregistré et le ménage.
func (s *Service) runBackup(ctx context.Context, t *backupTarget, b *domain.Backup) error {
	up := &backupTransfer{key: b.ObjectKey, upload: true}
	backupTransfers.Store(b.ID, up)
	defer backupTransfers.Delete(b.ID)

	err := s.Backups.Runner.Dump(ctx, s.task(t, b.ID, "up"))
	up.mu.Lock()
	size, received := up.size, up.done
	up.mu.Unlock()
	if err == nil && !received {
		err = errors.New("the backup did not reach the API")
	}
	keep := context.WithoutCancel(ctx)
	at := s.Backups.now()
	b.FinishedAt = &at
	if err != nil {
		b.Status, b.Error = domain.BackupFailed, lastLine(err.Error())
		s.deleteObject(keep, b.ObjectKey)
	} else {
		b.Status, b.SizeBytes = domain.BackupCompleted, size
	}
	if serr := s.Backups.Store.SaveBackup(keep, b); serr != nil {
		s.warn("addon backup: result not recorded", "addon", t.addon.ID, "error", serr)
	}
	if err != nil {
		s.warn("addon backup failed", "addon", t.addon.ID, "error", err)
		return err
	}
	s.pruneBackups(keep, t.addon.ID)
	return nil
}

// Restore : copie de l'état courant, puis la sauvegarde est rechargée (la
// base « app » est recréée). Synchrone : la console attend la fin.
func (s *Service) Restore(ctx context.Context, projectID, id, backupID uuid.UUID, confirm, actor string) (*domain.Backup, error) {
	t, err := s.backupTarget(ctx, projectID, id, true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(confirm) != t.addon.Name {
		return nil, domain.ErrConfirmMismatch
	}
	src, err := s.Backups.Store.GetBackup(ctx, t.addon.ID, backupID)
	if err != nil {
		return nil, err
	}
	if src.Status != domain.BackupCompleted {
		return nil, domain.ErrBackupNotRestorable
	}
	unlock, ok := tryLockAddon(t.addon.ID)
	if !ok {
		return nil, domain.ErrBackupRunning
	}
	defer unlock()

	// Filet : l'état courant d'abord. Sans copie, rien n'est remplacé.
	safety, err := s.newBackup(ctx, t, domain.TriggerBeforeRestore, actor)
	if err != nil {
		return nil, err
	}
	if err := s.runBackup(ctx, t, safety); err != nil {
		return nil, fmt.Errorf("the current data could not be copied, nothing was restored: %w", err)
	}

	taskID := uuid.New()
	backupTransfers.Store(taskID, &backupTransfer{key: src.ObjectKey})
	defer backupTransfers.Delete(taskID)
	if err := s.Backups.Runner.Restore(ctx, s.task(t, taskID, "down")); err != nil {
		return nil, fmt.Errorf("the restore failed (a copy of the data before it is kept): %w", err)
	}
	at := s.Backups.now()
	src.RestoredAt = &at
	if err := s.Backups.Store.SaveBackup(context.WithoutCancel(ctx), src); err != nil {
		s.warn("addon backup: restore not recorded", "backup", src.ID, "error", err)
	}
	return src, nil
}

// ReceiveBackup : la copie envoyée par le Job (URL signée, une fois).
func (s *Service) ReceiveBackup(ctx context.Context, taskID uuid.UUID, expires int64, sig string, r io.Reader, size int64) error {
	if !s.Backups.ready() || !s.Backups.Signer.VerifyAddonBackup(taskID, "up", expires, sig, s.Backups.now()) {
		return domain.ErrBackupNotFound
	}
	v, ok := backupTransfers.Load(taskID)
	if !ok || !v.(*backupTransfer).upload {
		return domain.ErrBackupNotFound
	}
	up := v.(*backupTransfer)
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.done {
		return domain.ErrBackupNotFound
	}
	max := s.Backups.maxBytes()
	if size > max {
		return domain.ErrBackupTooLarge
	}
	counter := &countingReader{r: io.LimitReader(r, max+1)}
	if err := s.Backups.Objects.Put(ctx, up.key, counter, size, "application/gzip"); err != nil {
		return err
	}
	if counter.n > max || counter.n == 0 {
		s.deleteObject(ctx, up.key)
		return domain.ErrBackupTooLarge
	}
	up.size, up.done = counter.n, true
	return nil
}

// OpenBackup : la sauvegarde lue par le Job de restauration (URL signée,
// pendant la tâche seulement).
func (s *Service) OpenBackup(ctx context.Context, taskID uuid.UUID, expires int64, sig string) (io.ReadCloser, int64, error) {
	if !s.Backups.ready() || !s.Backups.Signer.VerifyAddonBackup(taskID, "down", expires, sig, s.Backups.now()) {
		return nil, 0, domain.ErrBackupNotFound
	}
	v, ok := backupTransfers.Load(taskID)
	if !ok || v.(*backupTransfer).upload {
		return nil, 0, domain.ErrBackupNotFound
	}
	obj, info, err := s.Backups.Objects.Get(ctx, v.(*backupTransfer).key)
	if err != nil {
		return nil, 0, domain.ErrBackupNotFound
	}
	return obj, info.Size, nil
}

// pruneBackups : garde les BackupsKept dernières réussies, et les échecs
// d'une semaine.
func (s *Service) pruneBackups(ctx context.Context, addonID uuid.UUID) {
	list, err := s.Backups.Store.ListBackups(ctx, addonID)
	if err != nil {
		s.warn("addon backup: prune skipped", "addon", addonID, "error", err)
		return
	}
	kept, now := 0, s.Backups.now()
	for _, b := range list {
		drop := false
		switch b.Status {
		case domain.BackupCompleted:
			kept++
			drop = kept > domain.BackupsKept
		case domain.BackupFailed:
			drop = now.Sub(b.CreatedAt) > failedKept
		case domain.BackupRunning:
			drop = now.Sub(b.CreatedAt) > failedKept // reste d'une API relancée
		}
		if !drop {
			continue
		}
		s.deleteObject(ctx, b.ObjectKey)
		if err := s.Backups.Store.DeleteBackup(ctx, b.ID); err != nil {
			s.warn("addon backup: row not deleted", "backup", b.ID, "error", err)
		}
	}
}

// dropBackups : les copies d'un service supprimé (les lignes suivent la
// suppression du service).
func (s *Service) dropBackups(ctx context.Context, addonID uuid.UUID) {
	if !s.Backups.ready() {
		return
	}
	if err := s.Backups.Objects.DeletePrefix(ctx, "addon-backups/"+addonID.String()+"/"); err != nil {
		s.warn("addon backups: objects not deleted", "addon", addonID, "error", err)
	}
}

func (s *Service) deleteObject(ctx context.Context, key string) {
	if err := s.Backups.Objects.Delete(ctx, key); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		s.warn("addon backup: object not deleted", "key", key, "error", err)
	}
}

// RunBackups : sauvegarde nocturne. Un passage peu après le démarrage, puis
// toutes les `every` ; chaque service sauvegardable sans copie depuis 23 h
// en reçoit une (les échecs sont retentés après 3 h).
func (s *Service) RunBackups(ctx context.Context, every time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Minute):
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.BackupPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// BackupPass : un passage de la sauvegarde nocturne.
func (s *Service) BackupPass(ctx context.Context) {
	if !s.Backups.ready() {
		return
	}
	var templates []string
	for _, t := range domain.Catalog {
		if t.Backup != nil {
			templates = append(templates, t.ID)
		}
	}
	if len(templates) == 0 {
		return
	}
	due, err := s.Backups.Store.DueBackups(ctx, templates, s.Backups.now())
	if err != nil {
		s.warn("addon backups: due lookup failed", "error", err)
		return
	}
	for _, a := range due {
		if ctx.Err() != nil {
			return
		}
		t, err := s.backupTarget(ctx, a.ProjectID, a.ID, true)
		if err != nil {
			continue // service arrêté ou en cours de démarrage : au passage suivant
		}
		unlock, ok := tryLockAddon(a.ID)
		if !ok {
			continue
		}
		func() {
			defer unlock()
			b, err := s.newBackup(ctx, t, domain.TriggerNightly, "")
			if err != nil {
				s.warn("addon backup: not recorded", "addon", a.ID, "error", err)
				return
			}
			run, cancel := context.WithTimeout(ctx, backupTimeout+time.Minute)
			defer cancel()
			_ = s.runBackup(run, t, b)
		}()
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// ── Stockage ────────────────────────────────────────────────────────────

func (r *GormRepository) CreateBackup(ctx context.Context, b *domain.Backup) error {
	return r.DB.WithContext(ctx).Create(b).Error
}

func (r *GormRepository) SaveBackup(ctx context.Context, b *domain.Backup) error {
	return r.DB.WithContext(ctx).Save(b).Error
}

func (r *GormRepository) ListBackups(ctx context.Context, addonID uuid.UUID) ([]domain.Backup, error) {
	var out []domain.Backup
	err := r.DB.WithContext(ctx).Where("addon_id = ?", addonID).Order("created_at DESC").Find(&out).Error
	return out, err
}

func (r *GormRepository) GetBackup(ctx context.Context, addonID, id uuid.UUID) (*domain.Backup, error) {
	var b domain.Backup
	err := r.DB.WithContext(ctx).Where("addon_id = ? AND id = ?", addonID, id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrBackupNotFound
	}
	return &b, err
}

func (r *GormRepository) DeleteBackup(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Delete(&domain.Backup{}, "id = ?", id).Error
}

func (r *GormRepository) DueBackups(ctx context.Context, templates []string, now time.Time) ([]domain.Addon, error) {
	var out []domain.Addon
	err := r.DB.WithContext(ctx).Table("project_addons a").Select("a.*").
		Joins("JOIN projects p ON p.id = a.project_id AND p.deleted_at IS NULL AND p.status IN ?", []string{"READY", "ACTIVE"}).
		Where("a.template IN ?", templates).
		Where(`NOT EXISTS (SELECT 1 FROM addon_backups b WHERE b.addon_id = a.id AND (
			(b.status = 'completed' AND b.created_at > ?) OR
			(b.status = 'running' AND b.created_at > ?) OR
			(b.status = 'failed' AND b.created_at > ?)))`, now.Add(-23*time.Hour), now.Add(-backupStale), now.Add(-3*time.Hour)).
		Order("a.created_at").Scan(&out).Error
	return out, err
}
