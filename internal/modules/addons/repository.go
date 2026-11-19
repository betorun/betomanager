package addons

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
)

// GormRepository : tables project_addons et addon_attachments.
type GormRepository struct{ DB *gorm.DB }

var _ Repository = (*GormRepository)(nil)

func (r *GormRepository) Create(ctx context.Context, a *domain.Addon) error {
	err := r.DB.WithContext(ctx).Create(a).Error
	if err != nil && strings.Contains(err.Error(), "uq_project_addons_name") {
		return domain.ErrNameTaken // création concurrente du même nom
	}
	return err
}

func (r *GormRepository) List(ctx context.Context, projectID uuid.UUID) ([]domain.Addon, error) {
	var out []domain.Addon
	return out, r.DB.WithContext(ctx).Where("project_id = ?", projectID).Order("created_at").Find(&out).Error
}

func (r *GormRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.Addon, error) {
	var a domain.Addon
	err := r.DB.WithContext(ctx).Where("project_id = ? AND id = ?", projectID, id).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	return &a, err
}

func (r *GormRepository) Count(ctx context.Context, projectID uuid.UUID) (int64, error) {
	var n int64
	return n, r.DB.WithContext(ctx).Model(&domain.Addon{}).Where("project_id = ?", projectID).Count(&n).Error
}

func (r *GormRepository) SetVersion(ctx context.Context, id uuid.UUID, version string) error {
	return r.DB.WithContext(ctx).Model(&domain.Addon{}).Where("id = ?", id).Update("version", version).Error
}

func (r *GormRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Addon{}).Error
}

// SaveAttachment : un nouveau branchement du même conteneur sur la même
// variable remplace l'ancien (date et nom à jour).
func (r *GormRepository) SaveAttachment(ctx context.Context, a *domain.Attachment) error {
	return r.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "addon_id"}, {Name: "workload_id"}, {Name: "variable"}},
		DoUpdates: clause.AssignmentColumns([]string{"workload_name", "created_at"}),
	}).Create(a).Error
}

func (r *GormRepository) ListAttachments(ctx context.Context, addonIDs []uuid.UUID) ([]domain.Attachment, error) {
	out := []domain.Attachment{}
	if len(addonIDs) == 0 {
		return out, nil
	}
	return out, r.DB.WithContext(ctx).Where("addon_id IN ?", addonIDs).Order("created_at").Find(&out).Error
}
