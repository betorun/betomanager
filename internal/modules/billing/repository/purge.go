package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Purges : suppression des données des organisations suspendues (ADR 0012).
type Purges interface {
	// Suspended : organisations facturées et suspendues, données pas encore supprimées.
	Suspended(ctx context.Context) ([]PurgeRow, error)
	// SetStage : dernier avertissement envoyé (ou suppression faite).
	SetStage(ctx context.Context, org uuid.UUID, stage int, at time.Time) error
	// SetPurged : données supprimées.
	SetPurged(ctx context.Context, org uuid.UUID, at time.Time) error
	// PurgeState : étape et date de suppression d'une organisation.
	PurgeState(ctx context.Context, org uuid.UUID) (stage int, purgedAt *time.Time, err error)
}

// PurgeRow : une organisation suspendue.
type PurgeRow struct {
	OrganizationID uuid.UUID
	SuspendedAt    time.Time
	PurgeStage     int
	PurgeNoticedAt *time.Time
}

type purgeStore struct{ db *gorm.DB }

// NewPurges : stockage de la suppression des données.
func NewPurges(db *gorm.DB) Purges { return &purgeStore{db: db} }

func (s *purgeStore) Suspended(ctx context.Context) ([]PurgeRow, error) {
	var out []PurgeRow
	err := s.db.WithContext(ctx).Raw(`SELECT organization_id, suspended_at, purge_stage, purge_noticed_at FROM billing_accounts
		WHERE charging AND suspended_at IS NOT NULL AND data_purged_at IS NULL ORDER BY suspended_at`).Scan(&out).Error
	return out, err
}

func (s *purgeStore) SetStage(ctx context.Context, org uuid.UUID, stage int, at time.Time) error {
	return s.db.WithContext(ctx).Exec(`UPDATE billing_accounts SET purge_stage = ?, purge_noticed_at = ?, updated_at = NOW()
		WHERE organization_id = ?`, stage, at, org).Error
}

func (s *purgeStore) SetPurged(ctx context.Context, org uuid.UUID, at time.Time) error {
	return s.db.WithContext(ctx).Exec(`UPDATE billing_accounts SET data_purged_at = ?, updated_at = NOW() WHERE organization_id = ?`,
		at, org).Error
}

func (s *purgeStore) PurgeState(ctx context.Context, org uuid.UUID) (int, *time.Time, error) {
	var rows []struct {
		PurgeStage   int
		DataPurgedAt *time.Time
	}
	err := s.db.WithContext(ctx).Raw(`SELECT purge_stage, data_purged_at FROM billing_accounts WHERE organization_id = ?`, org).
		Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, nil, err
	}
	return rows[0].PurgeStage, rows[0].DataPurgedAt, nil
}
