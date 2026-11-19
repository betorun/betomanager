// Package repository : commandes de domaines (domain_orders).
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
)

// Store : commandes de domaines.
type Store interface {
	Create(ctx context.Context, o *domain.Order) error
	Get(ctx context.Context, org, id uuid.UUID) (*domain.Order, error)
	List(ctx context.Context, org uuid.UUID) ([]domain.Order, error)
	// Held : le nom est déjà détenu ou en commande (hors échecs).
	Held(ctx context.Context, name string) (bool, error)
	ByStatus(ctx context.Context, status string, limit int) ([]domain.Order, error)
	// RenewDue : domaines actifs à renouveler (expiration dans moins de before,
	// période pas encore payée).
	RenewDue(ctx context.Context, now time.Time, before time.Duration, limit int) ([]domain.Order, error)
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
}

type store struct{ db *gorm.DB }

// New : stockage en base.
func New(db *gorm.DB) Store { return &store{db: db} }

func (s *store) Create(ctx context.Context, o *domain.Order) error {
	return s.db.WithContext(ctx).Create(o).Error
}

func (s *store) Get(ctx context.Context, org, id uuid.UUID) (*domain.Order, error) {
	var o domain.Order
	err := s.db.WithContext(ctx).Where("organization_id = ? AND id = ?", org, id).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrOrderNotFound
	}
	return &o, err
}

func (s *store) List(ctx context.Context, org uuid.UUID) ([]domain.Order, error) {
	out := []domain.Order{}
	err := s.db.WithContext(ctx).Where("organization_id = ?", org).Order("created_at DESC").Find(&out).Error
	return out, err
}

func (s *store) Held(ctx context.Context, name string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.Order{}).
		Where("domain = ? AND status NOT IN ?", name, []string{domain.StatusFailed, domain.StatusExpired}).Count(&n).Error
	return n > 0, err
}

func (s *store) ByStatus(ctx context.Context, status string, limit int) ([]domain.Order, error) {
	var out []domain.Order
	err := s.db.WithContext(ctx).Where("status = ?", status).Order("created_at").Limit(limit).Find(&out).Error
	return out, err
}

func (s *store) RenewDue(ctx context.Context, now time.Time, before time.Duration, limit int) ([]domain.Order, error) {
	var out []domain.Order
	err := s.db.WithContext(ctx).
		Where("status = ? AND auto_renew AND expires_at IS NOT NULL AND expires_at <= ? AND (paid_until IS NULL OR paid_until < expires_at + interval '1 day')",
			domain.StatusActive, now.Add(before)).
		Order("expires_at").Limit(limit).Find(&out).Error
	return out, err
}

func (s *store) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	fields["updated_at"] = time.Now()
	return s.db.WithContext(ctx).Model(&domain.Order{}).Where("id = ?", id).Updates(fields).Error
}
