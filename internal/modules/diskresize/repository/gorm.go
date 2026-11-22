package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/diskresize/domain"
)

//	Stockage
//
// GormStore : opérations en base.
type GormStore struct{ DB *gorm.DB }

func (g *GormStore) Create(ctx context.Context, r *domain.Resize) error {
	return g.DB.WithContext(ctx).Create(r).Error
}

func (g *GormStore) Save(ctx context.Context, r *domain.Resize) error {
	return g.DB.WithContext(ctx).Save(r).Error
}

func (g *GormStore) Last(ctx context.Context, workload uuid.UUID) (*domain.Resize, error) {
	var r domain.Resize
	err := g.DB.WithContext(ctx).Where("workload_id = ?", workload).Order("created_at DESC").First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}
