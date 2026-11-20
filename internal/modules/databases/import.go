package databases

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Import d'un dump : envoi à l'API (stockage d'objets), puis Job du projet
// (voir import_job.go). L'état est relevé à la lecture : pas de workflow,
// un Job suffit et Kubernetes le relance pas (backoffLimit 0).

// dumpURLValidity : durée de l'URL signée (téléchargement par le Job).
const dumpURLValidity = 2 * time.Hour

// ImportStore : imports en base.
type ImportStore interface {
	CreateImport(ctx context.Context, i *domain.DatabaseImport) error
	GetImport(ctx context.Context, databaseID, id uuid.UUID) (*domain.DatabaseImport, error)
	GetImportByID(ctx context.Context, id uuid.UUID) (*domain.DatabaseImport, error)
	ListImports(ctx context.Context, databaseID uuid.UUID, limit int) ([]domain.DatabaseImport, error)
	FinishImport(ctx context.Context, id uuid.UUID, status, failure string, at time.Time) error
	SetImportStatus(ctx context.Context, id uuid.UUID, status string) error
}

// DumpSigner : URL signées des dumps (registry.Credentials).
type DumpSigner interface {
	DumpSignature(importID uuid.UUID, expires int64) string
	VerifyDump(importID uuid.UUID, expires int64, sig string, now time.Time) bool
	SnapshotSignature(taskID uuid.UUID, expires int64) string
	VerifySnapshot(taskID uuid.UUID, expires int64, sig string, now time.Time) bool
}

// ImportConfig : ce qu'il faut pour importer (absent : fonction indisponible).
type ImportConfig struct {
	Store    ImportStore
	Dumps    objectstore.Store
	Signer   DumpSigner
	Importer Importer
	// PublicAPIURL : API vue par les pods (ex. http://10.42.0.1:18080/kmanager/v1).
	PublicAPIURL string
	FetchImage   string
	EgressAllow  []string
	MaxBytes     int64
	Now          func() time.Time
}

func (c *ImportConfig) ready() bool {
	return c != nil && c.Store != nil && c.Dumps != nil && c.Signer != nil && c.Importer != nil && c.PublicAPIURL != "" && c.FetchImage != ""
}

func (c *ImportConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// MaxDumpBytes : taille maximale d'un dump.
func (s *Service) MaxDumpBytes() int64 {
	if s.Imports != nil && s.Imports.MaxBytes > 0 {
		return s.Imports.MaxBytes
	}
	return domain.DefaultMaxDumpBytes
}

func dumpKey(id uuid.UUID) string { return "database-imports/" + id.String() + ".pgc" }

// Import : charge un dump (pg_dump -Fc) dans la base. clean : objets
// existants du dump supprimés d'abord (sinon, un dump sur une base non vide
// échoue au premier objet déjà présent, sans rien casser). reset : la base
// est d'abord vidée, après une copie de sécurité (voir Reset).
func (s *Service) Import(ctx context.Context, projectID, databaseID uuid.UUID, r io.Reader, size int64, clean, reset bool, creator string) (*domain.DatabaseImport, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if !s.Imports.ready() {
		return nil, domain.ErrImportsDisabled
	}
	if size > s.MaxDumpBytes() {
		return nil, domain.ErrDumpTooLarge
	}
	d, err := s.Repo.Get(ctx, projectID, databaseID)
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
		return nil, notReady(st)
	}
	if running, err := s.runningImport(ctx, ns, databaseID); err != nil {
		return nil, err
	} else if running {
		return nil, domain.ErrImportRunning
	}

	br := bufio.NewReader(r)
	head, _ := br.Peek(5)
	if !domain.IsCustomDump(head) {
		return nil, domain.ErrNotADump
	}
	if reset {
		if err := s.resetReady(); err != nil {
			return nil, err
		}
		if _, err := s.reset(ctx, &target{db: d, ns: ns, TaskTarget: TaskTarget{Cluster: d.ClusterName(), Image: st.Image}}, creator); err != nil {
			return nil, err
		}
	}

	imp := &domain.DatabaseImport{ID: uuid.New(), DatabaseID: databaseID, ProjectID: projectID, Status: domain.ImportPending,
		Clean: clean, SizeBytes: size, CreatedBy: creator, CreatedAt: s.Imports.now()}
	if err := s.Imports.Dumps.Put(ctx, dumpKey(imp.ID), br, size, "application/octet-stream"); err != nil {
		return nil, fmt.Errorf("store dump: %w", err)
	}
	if err := s.Imports.Store.CreateImport(ctx, imp); err != nil {
		_ = s.Imports.Dumps.Delete(ctx, dumpKey(imp.ID))
		return nil, err
	}
	exp := s.Imports.now().Add(dumpURLValidity).Unix()
	spec := ImportJobSpec{
		ImportID: imp.ID.String(), Cluster: d.ClusterName(), Image: st.Image, FetchImage: s.Imports.FetchImage, Clean: clean,
		DumpURL: fmt.Sprintf("%s/databases/imports/%s/dump?expires=%d&sig=%s",
			strings.TrimRight(s.Imports.PublicAPIURL, "/"), imp.ID, exp, s.Imports.Signer.DumpSignature(imp.ID, exp)),
		EgressAllow: s.Imports.EgressAllow,
	}
	if err := s.Imports.Importer.StartImport(ctx, ns, spec); err != nil {
		s.finish(ctx, imp, domain.ImportFailed, "the import could not be started")
		return nil, err
	}
	if err := s.Imports.Store.SetImportStatus(ctx, imp.ID, domain.ImportRunning); err != nil {
		return nil, err
	}
	imp.Status = domain.ImportRunning
	return imp, nil
}

// runningImport : un import non terminé (état relevé au passage).
func (s *Service) runningImport(ctx context.Context, ns string, databaseID uuid.UUID) (bool, error) {
	list, err := s.Imports.Store.ListImports(ctx, databaseID, 5)
	if err != nil {
		return false, err
	}
	for i := range list {
		if list[i].Done() {
			continue
		}
		if s.refresh(ctx, ns, &list[i]); !list[i].Done() {
			return true, nil
		}
	}
	return false, nil
}

// GetImport : un import, état à jour.
func (s *Service) GetImport(ctx context.Context, projectID, databaseID, id uuid.UUID) (*domain.DatabaseImport, error) {
	if !s.Imports.ready() {
		return nil, domain.ErrImportsDisabled
	}
	if _, err := s.Repo.Get(ctx, projectID, databaseID); err != nil {
		return nil, err
	}
	imp, err := s.Imports.Store.GetImport(ctx, databaseID, id)
	if err != nil {
		return nil, err
	}
	if !imp.Done() {
		ns, err := s.namespace(ctx, projectID)
		if err != nil {
			return nil, err
		}
		s.refresh(ctx, ns, imp)
	}
	return imp, nil
}

// ListImports : imports récents d'une base, les plus récents d'abord.
func (s *Service) ListImports(ctx context.Context, projectID, databaseID uuid.UUID) ([]domain.DatabaseImport, error) {
	if !s.Imports.ready() {
		return nil, domain.ErrImportsDisabled
	}
	if _, err := s.Repo.Get(ctx, projectID, databaseID); err != nil {
		return nil, err
	}
	list, err := s.Imports.Store.ListImports(ctx, databaseID, 10)
	if err != nil {
		return nil, err
	}
	var ns string
	for i := range list {
		if list[i].Done() {
			continue
		}
		if ns == "" {
			if ns, err = s.namespace(ctx, projectID); err != nil {
				return nil, err
			}
		}
		s.refresh(ctx, ns, &list[i])
	}
	return list, nil
}

// refresh : état du Job reporté sur l'import ; terminé, le dump est effacé.
// Au mieux : une erreur de lecture laisse l'import tel quel.
func (s *Service) refresh(ctx context.Context, ns string, imp *domain.DatabaseImport) {
	st, err := s.Imports.Importer.ImportState(ctx, ns, imp.ID.String())
	if err != nil {
		s.warn("import state unavailable", "import", imp.ID, "error", err)
		return
	}
	switch {
	case st.Done && st.Failed:
		s.finish(ctx, imp, domain.ImportFailed, st.Log)
	case st.Done:
		s.finish(ctx, imp, domain.ImportSucceeded, "")
	case !st.Exists && imp.Status == domain.ImportRunning:
		// Job disparu sans issue connue (supprimé, ou nettoyé après 24 h).
		s.finish(ctx, imp, domain.ImportFailed, "the import job no longer exists")
	}
}

func (s *Service) finish(ctx context.Context, imp *domain.DatabaseImport, status, failure string) {
	at := s.Imports.now()
	if err := s.Imports.Store.FinishImport(ctx, imp.ID, status, failure, at); err != nil {
		s.warn("import status not recorded", "import", imp.ID, "error", err)
		return
	}
	imp.Status, imp.Failure, imp.FinishedAt = status, failure, &at
	if err := s.Imports.Dumps.Delete(ctx, dumpKey(imp.ID)); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		s.warn("dump not deleted", "import", imp.ID, "error", err)
	}
}

// Dump : fichier d'un import en cours, pour le Job (URL signée).
func (s *Service) Dump(ctx context.Context, id uuid.UUID, expires int64, sig string) (io.ReadCloser, int64, error) {
	if !s.Imports.ready() || !s.Imports.Signer.VerifyDump(id, expires, sig, s.Imports.now()) {
		return nil, 0, domain.ErrImportNotFound
	}
	imp, err := s.Imports.Store.GetImportByID(ctx, id)
	if err != nil || imp.Done() {
		return nil, 0, domain.ErrImportNotFound
	}
	obj, info, err := s.Imports.Dumps.Get(ctx, dumpKey(id))
	if err != nil {
		return nil, 0, domain.ErrImportNotFound
	}
	return obj, info.Size, nil
}

func (s *Service) warn(msg string, kv ...any) {
	if s.Logger != nil {
		s.Logger.Warnw(msg, kv...)
	}
}

// ── Stockage ────────────────────────────────────────────────────────────

func (r *GormRepository) CreateImport(ctx context.Context, i *domain.DatabaseImport) error {
	return r.DB.WithContext(ctx).Create(i).Error
}

func (r *GormRepository) GetImport(ctx context.Context, databaseID, id uuid.UUID) (*domain.DatabaseImport, error) {
	var i domain.DatabaseImport
	err := r.DB.WithContext(ctx).Where("database_id = ? AND id = ?", databaseID, id).First(&i).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrImportNotFound
	}
	return &i, err
}

func (r *GormRepository) GetImportByID(ctx context.Context, id uuid.UUID) (*domain.DatabaseImport, error) {
	var i domain.DatabaseImport
	err := r.DB.WithContext(ctx).Where("id = ?", id).First(&i).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrImportNotFound
	}
	return &i, err
}

func (r *GormRepository) ListImports(ctx context.Context, databaseID uuid.UUID, limit int) ([]domain.DatabaseImport, error) {
	out := []domain.DatabaseImport{}
	return out, r.DB.WithContext(ctx).Where("database_id = ?", databaseID).Order("created_at DESC").Limit(limit).Find(&out).Error
}

func (r *GormRepository) FinishImport(ctx context.Context, id uuid.UUID, status, failure string, at time.Time) error {
	return r.DB.WithContext(ctx).Model(&domain.DatabaseImport{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "failure": failure, "finished_at": at}).Error
}

func (r *GormRepository) SetImportStatus(ctx context.Context, id uuid.UUID, status string) error {
	return r.DB.WithContext(ctx).Model(&domain.DatabaseImport{}).Where("id = ?", id).Update("status", status).Error
}
