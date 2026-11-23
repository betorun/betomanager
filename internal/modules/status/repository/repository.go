// Package repository : incidents (status_incidents, status_incident_updates)
// et signaux des sondes, lus en SQL dans les tables des autres modules.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
	"github.com/thekrauss/kubemanager/internal/modules/status/service"
)

// GormStore : service.Store et service.Signals sur Postgres.
type GormStore struct{ DB *gorm.DB }

var (
	_ service.Store   = (*GormStore)(nil)
	_ service.Signals = (*GormStore)(nil)
)

func withUpdates(db *gorm.DB) *gorm.DB {
	return db.Preload("Updates", func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC, id ASC") })
}

func (s *GormStore) CreateIncident(ctx context.Context, i *domain.Incident) error {
	return s.DB.WithContext(ctx).Create(i).Error // messages créés avec (association)
}

func (s *GormStore) Incident(ctx context.Context, id uuid.UUID) (*domain.Incident, error) {
	var i domain.Incident
	err := withUpdates(s.DB.WithContext(ctx)).Where("id = ?", id).Take(&i).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrIncidentNotFound
	}
	return &i, err
}

func (s *GormStore) Incidents(ctx context.Context, open bool, limit int) ([]domain.Incident, error) {
	q := withUpdates(s.DB.WithContext(ctx)).Order("started_at DESC").Limit(limit)
	if open {
		q = q.Where("status <> ?", domain.StatusResolved)
	}
	out := []domain.Incident{}
	return out, q.Find(&out).Error
}

func (s *GormStore) Public(ctx context.Context, since time.Time) ([]domain.Incident, error) {
	out := []domain.Incident{}
	err := withUpdates(s.DB.WithContext(ctx)).
		Where("status <> ? OR resolved_at >= ?", domain.StatusResolved, since).
		Order("started_at DESC").Limit(100).Find(&out).Error
	return out, err
}

// SaveIncident : champs de l'incident seulement (pas ses messages).
func (s *GormStore) SaveIncident(ctx context.Context, i *domain.Incident) error {
	return s.DB.WithContext(ctx).Omit("Updates").Save(i).Error
}

// AddUpdate : le message et le nouveau statut, ensemble.
func (s *GormStore) AddUpdate(ctx context.Context, i *domain.Incident, u *domain.IncidentUpdate) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(u).Error; err != nil {
			return err
		}
		return tx.Model(&domain.Incident{}).Where("id = ?", i.ID).Updates(map[string]any{
			"status": i.Status, "resolved_at": i.ResolvedAt, "updated_by": i.UpdatedBy, "updated_at": i.UpdatedAt}).Error
	})
}

// ── Signaux des sondes ──────────────────────────────────────────────────

func (s *GormStore) VPSSignals(ctx context.Context) (domain.VPSSignals, error) {
	var out domain.VPSSignals
	db := s.DB.WithContext(ctx)
	var finished sql.NullTime
	var failed sql.NullBool
	err := db.Raw(`SELECT finished_at, error <> '' FROM compute_reconcile_runs
		WHERE finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 1`).Row().Scan(&finished, &failed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.LastFinished, out.LastFailed = ptr(finished), failed.Bool
	var first, pending, started sql.NullTime
	if err := db.Raw(`SELECT MIN(started_at) FROM compute_reconcile_runs`).Row().Scan(&first); err != nil {
		return out, err
	}
	if err := db.Raw(`SELECT MIN(created_at) FROM compute_requests WHERE status = 'pending'`).Row().Scan(&pending); err != nil {
		return out, err
	}
	if err := db.Raw(`SELECT MIN(started_at) FROM compute_requests WHERE status = 'started'`).Row().Scan(&started); err != nil {
		return out, err
	}
	out.FirstStarted, out.OldestPending, out.OldestStarted = ptr(first), ptr(pending), ptr(started)
	return out, nil
}

func (s *GormStore) OldestDomainOrdering(ctx context.Context) (*time.Time, error) {
	var t sql.NullTime
	err := s.DB.WithContext(ctx).Raw(`SELECT MIN(updated_at) FROM domain_orders WHERE status = 'ordering'`).Row().Scan(&t)
	return ptr(t), err
}

func ptr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
