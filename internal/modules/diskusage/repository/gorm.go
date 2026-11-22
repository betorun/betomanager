package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

// GormStore : mesures en base.
type GormStore struct{ DB *gorm.DB }

func (g *GormStore) ActiveProjects(ctx context.Context) ([]domain.Project, error) {
	var out []domain.Project
	err := g.DB.WithContext(ctx).Table("projects").Select("id, namespace").
		Where("status IN ? AND namespace <> '' AND deleted_at IS NULL", []string{"READY", "ACTIVE"}).Order("created_at").Scan(&out).Error
	return out, err
}

func (g *GormStore) Get(ctx context.Context, namespace, pvc string) (*domain.Usage, error) {
	var u domain.Usage
	err := g.DB.WithContext(ctx).Where("namespace = ? AND pvc = ?", namespace, pvc).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &u, err
}

func (g *GormStore) Save(ctx context.Context, u *domain.Usage) error {
	return g.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(u).Error
}

func (g *GormStore) Prune(ctx context.Context, namespace string, keep []string) error {
	q := g.DB.WithContext(ctx).Where("namespace = ?", namespace)
	if len(keep) > 0 {
		q = q.Where("pvc NOT IN ?", keep)
	}
	return q.Delete(&domain.Usage{}).Error
}

func (g *GormStore) ListProject(ctx context.Context, project uuid.UUID) ([]domain.Usage, error) {
	var out []domain.Usage
	err := g.DB.WithContext(ctx).Where("project_id = ?", project).Order("kind, target_name").Find(&out).Error
	return out, err
}
