package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

func (r *GormRepository) SaveArchive(ctx context.Context, a *domain.Archive) error {
	return r.DB.WithContext(ctx).Create(a).Error
}

func (r *GormRepository) ExpiredArchives(ctx context.Context, now time.Time) ([]domain.Archive, error) {
	var out []domain.Archive
	err := r.DB.WithContext(ctx).Where("expires_at <= ?", now).Order("expires_at").Limit(100).Find(&out).Error
	return out, err
}

func (r *GormRepository) DeleteArchive(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Archive{}).Error
}

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

func (r *GormRepository) MarkSnapshotRestore(ctx context.Context, databaseID, importID uuid.UUID) error {
	return r.DB.WithContext(ctx).Model(&domain.Snapshot{}).
		Where("database_id = ?", databaseID).Update("restore_import_id", importID).Error
}

func (r *GormRepository) DeleteSnapshot(ctx context.Context, databaseID uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("database_id = ?", databaseID).Delete(&domain.Snapshot{}).Error
}

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

// GormRepository : stockage Postgres.
type GormRepository struct{ DB *gorm.DB }

func (r *GormRepository) Create(ctx context.Context, d *domain.Database) error {
	err := r.DB.WithContext(ctx).Create(d).Error
	if err != nil && strings.Contains(err.Error(), "databases_project_name") {
		return domain.ErrNameTaken // création concurrente du même nom
	}
	return err
}

func (r *GormRepository) List(ctx context.Context, projectID uuid.UUID) ([]domain.Database, error) {
	var out []domain.Database
	err := r.DB.WithContext(ctx).Where("project_id = ?", projectID).Order("created_at").Find(&out).Error
	return out, err
}

func (r *GormRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.Database, error) {
	var d domain.Database
	err := r.DB.WithContext(ctx).Where("project_id = ? AND id = ?", projectID, id).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	return &d, err
}

func (r *GormRepository) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	return r.DB.WithContext(ctx).Model(&domain.Database{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()}).Error
}

func (r *GormRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Database{}).Error
}

func (r *GormRepository) SaveAttachment(ctx context.Context, a *domain.Attachment) error {
	return r.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(a).Error
}

// ListAttachments : branchements vers des conteneurs toujours présents.
func (r *GormRepository) ListAttachments(ctx context.Context, databaseID uuid.UUID) ([]domain.Attachment, error) {
	out := []domain.Attachment{}
	err := r.DB.WithContext(ctx).Table("database_attachments AS a").Select("a.*").
		Joins("JOIN workloads w ON w.id = a.workload_id").
		Where("a.database_id = ? AND w.status NOT IN ?", databaseID, []string{"DELETED", "DELETING"}).
		Order("a.workload_name, a.variable").Scan(&out).Error
	return out, err
}

// StorageBytes : disques demandés par les bases du projet (vue d'ensemble).
func (r *GormRepository) StorageBytes(ctx context.Context, projectID uuid.UUID) (int64, error) {
	var sizes []string
	if err := r.DB.WithContext(ctx).Model(&domain.Database{}).Where("project_id = ?", projectID).Pluck("storage_size", &sizes).Error; err != nil {
		return 0, err
	}
	var total int64
	for _, sz := range sizes {
		if q, err := resource.ParseQuantity(sz); err == nil {
			total += q.Value()
		}
	}
	return total, nil
}
