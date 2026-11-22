// Package repository : VPS, offres, demandes (outbox) et secrets en base.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// GormStore : tables compute_*.
type GormStore struct{ DB *gorm.DB }

var _ service.Store = (*GormStore)(nil)

func (s *GormStore) Offers(ctx context.Context) ([]domain.Offer, error) {
	out := []domain.Offer{}
	return out, s.DB.WithContext(ctx).Order("position, code").Find(&out).Error
}

func (s *GormStore) Offer(ctx context.Context, code string) (*domain.Offer, error) {
	var o domain.Offer
	err := s.DB.WithContext(ctx).Where("code = ?", domain.NormalizeCode(code)).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrOfferNotFound
	}
	return &o, err
}

func (s *GormStore) SaveOffer(ctx context.Context, o *domain.Offer) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(o).Error
}

// EnqueueTx : demande en attente, dans la transaction de l'appelant.
func (s *GormStore) EnqueueTx(tx *gorm.DB, r *domain.Request) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.Status = domain.RequestPending
	if len(r.Payload) == 0 {
		r.Payload = []byte(`{}`)
	}
	return tx.Create(r).Error
}

// ClaimPending : FOR UPDATE SKIP LOCKED : deux relais ne prennent jamais la même demande.
func (s *GormStore) ClaimPending(ctx context.Context, limit int, fn func(tx *gorm.DB, rs []domain.Request) error) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rs []domain.Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", domain.RequestPending).Order("created_at").Limit(limit).Find(&rs).Error; err != nil {
			return err
		}
		if len(rs) == 0 {
			return nil
		}
		return fn(tx, rs)
	})
}

func (s *GormStore) MarkStarted(tx *gorm.DB, id uuid.UUID, at time.Time) error {
	return tx.Model(&domain.Request{}).Where("id = ? AND status = ?", id, domain.RequestPending).
		Updates(map[string]any{"status": domain.RequestStarted, "started_at": at, "attempts": gorm.Expr("attempts + 1")}).Error
}

// maxErrorLen : début du message gardé.
const maxErrorLen = 500

func (s *GormStore) MarkAttemptFailed(tx *gorm.DB, id uuid.UUID, errMsg string) error {
	if len(errMsg) > maxErrorLen {
		errMsg = errMsg[:maxErrorLen]
	}
	return tx.Model(&domain.Request{}).Where("id = ?", id).
		Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": errMsg}).Error
}

// FinishRequest : fin d'une demande lancée (par l'activité du workflow) ;
// idempotent : une demande déjà terminée ne change plus.
func (s *GormStore) FinishRequest(ctx context.Context, id uuid.UUID, ok bool, errMsg string, at time.Time) error {
	status := domain.RequestDone
	if !ok {
		status = domain.RequestFailed
	}
	if len(errMsg) > maxErrorLen {
		errMsg = errMsg[:maxErrorLen]
	}
	return s.DB.WithContext(ctx).Model(&domain.Request{}).
		Where("id = ? AND status IN ?", id, []string{domain.RequestPending, domain.RequestStarted}).
		Updates(map[string]any{"status": status, "finished_at": at, "last_error": errMsg}).Error
}

func (s *GormStore) Request(ctx context.Context, id uuid.UUID) (*domain.Request, error) {
	var r domain.Request
	err := s.DB.WithContext(ctx).Where("id = ?", id).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

// ── Réconciliation ──────────────────────────────────────────────────────

func (s *GormStore) ProviderVPS(ctx context.Context, provider string) ([]domain.VPS, error) {
	out := []domain.VPS{}
	return out, s.DB.WithContext(ctx).Where("provider = ? AND provider_id <> ''", provider).Find(&out).Error
}

// ApplyObserved : écrit seulement si la version n'a pas bougé (une opération
// du client passée entre-temps garde la main).
func (s *GormStore) ApplyObserved(ctx context.Context, id uuid.UUID, version int64, state, ipv4, ipv6 string, at time.Time) (bool, error) {
	res := s.DB.WithContext(ctx).Model(&domain.VPS{}).Where("id = ? AND version = ?", id, version).
		Updates(map[string]any{"state": state, "ipv4": ipv4, "ipv6": ipv6, "version": gorm.Expr("version + 1"), "updated_at": at})
	return res.RowsAffected == 1, res.Error
}

// ReplaceMachines : le relevé du fournisseur remplace le précédent, d'un bloc.
func (s *GormStore) ReplaceMachines(ctx context.Context, provider string, ms []domain.ProviderMachine) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("provider = ?", provider).Delete(&domain.ProviderMachine{}).Error; err != nil {
			return err
		}
		if len(ms) == 0 {
			return nil
		}
		return tx.CreateInBatches(ms, 200).Error
	})
}

func (s *GormStore) SaveRun(ctx context.Context, r *domain.ReconcileRun) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(r).Error
}

// ── Back-office ─────────────────────────────────────────────────────────

func (s *GormStore) ListVPS(ctx context.Context, limit int) ([]domain.VPS, error) {
	out := []domain.VPS{}
	return out, s.DB.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&out).Error
}

func (s *GormStore) Machines(ctx context.Context) ([]domain.ProviderMachine, error) {
	out := []domain.ProviderMachine{}
	return out, s.DB.WithContext(ctx).Order("provider, provider_id").Find(&out).Error
}

func (s *GormStore) Runs(ctx context.Context, limit int) ([]domain.ReconcileRun, error) {
	out := []domain.ReconcileRun{}
	return out, s.DB.WithContext(ctx).Order("started_at DESC").Limit(limit).Find(&out).Error
}

func (s *GormStore) OrganizationNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID          uuid.UUID
		DisplayName string
	}
	if err := s.DB.WithContext(ctx).Table("organizations").Select("id, display_name").Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.DisplayName
	}
	return out, nil
}
