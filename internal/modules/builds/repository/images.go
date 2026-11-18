package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
)

// ImageStore : images construites encore présentes dans le registre.
type ImageStore interface {
	SetImageSize(ctx context.Context, id uuid.UUID, size int64) error
	// Images : builds réussis d'un dépôt dont l'image est encore au registre,
	// du plus récent au plus ancien.
	Images(ctx context.Context, project uuid.UUID, name string) ([]domain.Build, error)
	MarkPruned(ctx context.Context, id uuid.UUID) error
	// Usage : octets occupés par les images du projet (un même digest compté
	// une fois par dépôt).
	Usage(ctx context.Context, project uuid.UUID) (int64, error)
	// Names : dépôts du projet qui ont encore des images au registre.
	Names(ctx context.Context, project uuid.UUID) ([]string, error)
	// WorkloadImages : images des conteneurs du projet (jamais retirées).
	WorkloadImages(ctx context.Context, project uuid.UUID) ([]string, error)
}

type imageStore struct{ db *gorm.DB }

// NewImageStore : stockage en base.
func NewImageStore(db *gorm.DB) ImageStore { return &imageStore{db: db} }

func (s *imageStore) SetImageSize(ctx context.Context, id uuid.UUID, size int64) error {
	return s.db.WithContext(ctx).Model(&domain.Build{}).Where("id = ?", id).Update("image_size", size).Error
}

func (s *imageStore) Images(ctx context.Context, project uuid.UUID, name string) ([]domain.Build, error) {
	var rows []domain.Build
	err := s.db.WithContext(ctx).
		Where("project_id = ? AND name = ? AND status = ? AND image_pruned_at IS NULL AND digest <> ''",
			project, name, domain.StatusSucceeded).
		Order("finished_at DESC NULLS LAST, created_at DESC").Find(&rows).Error
	return rows, err
}

func (s *imageStore) MarkPruned(ctx context.Context, id uuid.UUID) error {
	return s.db.WithContext(ctx).Model(&domain.Build{}).Where("id = ?", id).Update("image_pruned_at", time.Now()).Error
}

func (s *imageStore) Usage(ctx context.Context, project uuid.UUID) (int64, error) {
	var total int64
	err := s.db.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(size), 0) FROM (
			SELECT DISTINCT ON (name, digest) image_size AS size FROM builds
			WHERE project_id = ? AND status = ? AND image_pruned_at IS NULL AND digest <> ''
		) AS images`, project, domain.StatusSucceeded).Scan(&total).Error
	return total, err
}

func (s *imageStore) WorkloadImages(ctx context.Context, project uuid.UUID) ([]string, error) {
	var images []string
	err := s.db.WithContext(ctx).Raw(`SELECT image FROM workloads WHERE project_id = ? AND image <> ''`, project).
		Scan(&images).Error
	return images, err
}

func (s *imageStore) Names(ctx context.Context, project uuid.UUID) ([]string, error) {
	var names []string
	err := s.db.WithContext(ctx).Raw(`SELECT DISTINCT name FROM builds
		WHERE project_id = ? AND status = ? AND image_pruned_at IS NULL AND digest <> '' ORDER BY name`,
		project, domain.StatusSucceeded).Scan(&names).Error
	return names, err
}
