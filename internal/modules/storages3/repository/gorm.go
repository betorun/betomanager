// Package repository : buckets et clés en base, et le S3 de la plateforme.
package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

type GormRepository struct{ DB *gorm.DB }

func (r *GormRepository) CreateBucket(ctx context.Context, b *domain.Bucket) error {
	return r.DB.WithContext(ctx).Create(b).Error
}

func (r *GormRepository) ListBuckets(ctx context.Context, projectID uuid.UUID) ([]domain.Bucket, error) {
	var out []domain.Bucket
	err := r.DB.WithContext(ctx).Where("project_id = ?", projectID).Order("created_at").Find(&out).Error
	return out, err
}

func (r *GormRepository) GetBucket(ctx context.Context, projectID, id uuid.UUID) (*domain.Bucket, error) {
	var b domain.Bucket
	err := r.DB.WithContext(ctx).Where("project_id = ? AND id = ?", projectID, id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *GormRepository) UpdateBucket(ctx context.Context, b *domain.Bucket) error {
	return r.DB.WithContext(ctx).Model(b).Select("quota_bytes", "used_bytes", "usage_at", "over_quota", "public", "suspended", "status", "updated_at").Updates(b).Error
}

func (r *GormRepository) DeleteBucket(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Bucket{}).Error
}

func (r *GormRepository) AllBuckets(ctx context.Context) ([]domain.Bucket, error) {
	var out []domain.Bucket
	err := r.DB.WithContext(ctx).Where("status = ?", domain.StatusReady).Order("created_at").Find(&out).Error
	return out, err
}

func (r *GormRepository) ProjectBuckets(ctx context.Context, projectID uuid.UUID) ([]domain.Bucket, error) {
	var out []domain.Bucket
	err := r.DB.WithContext(ctx).Where("project_id = ?", projectID).Order("created_at").Find(&out).Error
	return out, err
}

// UpdateKey : rotation (nouvel identifiant public, date).
func (r *GormRepository) UpdateKey(ctx context.Context, k *domain.Key) error {
	return r.DB.WithContext(ctx).Model(k).Select("access_key_id", "rotated_at").Updates(k).Error
}

func (r *GormRepository) CreateKey(ctx context.Context, k *domain.Key) error {
	return r.DB.WithContext(ctx).Create(k).Error
}

func (r *GormRepository) ListKeys(ctx context.Context, bucketID uuid.UUID) ([]domain.Key, error) {
	var out []domain.Key
	err := r.DB.WithContext(ctx).Where("bucket_id = ?", bucketID).Order("created_at").Find(&out).Error
	return out, err
}

func (r *GormRepository) GetKey(ctx context.Context, bucketID, id uuid.UUID) (*domain.Key, error) {
	var k domain.Key
	err := r.DB.WithContext(ctx).Where("bucket_id = ? AND id = ?", bucketID, id).First(&k).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (r *GormRepository) DeleteKey(ctx context.Context, id uuid.UUID) error {
	return r.DB.WithContext(ctx).Where("id = ?", id).Delete(&domain.Key{}).Error
}
