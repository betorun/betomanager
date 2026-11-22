package repository

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
)

// PreferenceStore : préférences en base.
type PreferenceStore struct{ DB *gorm.DB }

type muteRow struct {
	UserID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	Category string    `gorm:"primaryKey"`
}

func (muteRow) TableName() string { return "alert_mutes" }

// Get : préférences d'une personne.
func (s *PreferenceStore) Get(ctx context.Context, user uuid.UUID) (*domain.Preferences, error) {
	out := &domain.Preferences{Alerts: map[string]bool{}}
	if err := s.DB.WithContext(ctx).Raw(`SELECT locale FROM users WHERE id = ?`, user).Scan(&out.Locale).Error; err != nil {
		return nil, err
	}
	var muted []string
	if err := s.DB.WithContext(ctx).Model(&muteRow{}).Where("user_id = ?", user).Pluck("category", &muted).Error; err != nil {
		return nil, err
	}
	for _, c := range domain.Categories {
		out.Alerts[c] = true
	}
	for _, c := range muted {
		if domain.ValidCategory(c) {
			out.Alerts[c] = false
		}
	}
	return out, nil
}

// Update : applique les changements demandés.
func (s *PreferenceStore) Update(ctx context.Context, user uuid.UUID, in domain.PreferencesUpdate) (*domain.Preferences, error) {
	for c := range in.Alerts {
		if !domain.ValidCategory(c) {
			return nil, domain.ErrUnknownCategory
		}
	}
	if in.Locale != nil && *in.Locale != "fr" && *in.Locale != "en" {
		return nil, domain.ErrInvalidLocale
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if in.Locale != nil {
			if err := tx.Exec(`UPDATE users SET locale = ? WHERE id = ?`, *in.Locale, user).Error; err != nil {
				return err
			}
		}
		for c, on := range in.Alerts {
			if err := setMuted(tx, user, c, !on); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, user)
}

func setMuted(tx *gorm.DB, user uuid.UUID, category string, muted bool) error {
	if muted {
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&muteRow{UserID: user, Category: category}).Error
	}
	return tx.Where("user_id = ? AND category = ?", user, category).Delete(&muteRow{}).Error
}

// Mute : coupe une catégorie (lien de désabonnement).
func (s *PreferenceStore) Mute(ctx context.Context, user uuid.UUID, category string) error {
	if !domain.ValidCategory(category) {
		return domain.ErrUnknownCategory
	}
	return setMuted(s.DB.WithContext(ctx), user, category, true)
}
