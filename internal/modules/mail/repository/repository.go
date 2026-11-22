package repository

import (
	"context"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
)

// GormRepository : table mail_suppressions.
type GormRepository struct{ DB *gorm.DB }

func (r *GormRepository) IsSuppressed(ctx context.Context, email string) (bool, error) {
	var n int64
	err := r.DB.WithContext(ctx).Model(&domain.Suppression{}).Where("email = ?", domain.NormalizeEmail(email)).Count(&n).Error
	return n > 0, err
}

// Suppress : la première raison connue est gardée (idempotent).
func (r *GormRepository) Suppress(ctx context.Context, s domain.Suppression) error {
	if s.Email == "" {
		return nil
	}
	return r.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&s).Error
}

// List : les plus récentes d'abord ; search filtre sur l'adresse.
func (r *GormRepository) List(ctx context.Context, search string, limit int) ([]domain.Suppression, error) {
	q := r.DB.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if s := strings.ToLower(strings.TrimSpace(search)); s != "" {
		q = q.Where("email LIKE ?", "%"+strings.NewReplacer("%", `\%`, "_", `\_`).Replace(s)+"%")
	}
	out := []domain.Suppression{}
	return out, q.Find(&out).Error
}

// Remove : rend vrai si l'adresse était mise de côté.
func (r *GormRepository) Remove(ctx context.Context, email string) (bool, error) {
	res := r.DB.WithContext(ctx).Where("email = ?", domain.NormalizeEmail(email)).Delete(&domain.Suppression{})
	return res.RowsAffected > 0, res.Error
}
