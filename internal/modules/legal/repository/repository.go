// Package repository : tables legal_documents et legal_acceptances (Postgres).
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/core/pgerr"
	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
	"github.com/thekrauss/kubemanager/internal/modules/legal/service"
)

// GormStore : service.Store sur Postgres.
type GormStore struct{ DB *gorm.DB }

var _ service.Store = (*GormStore)(nil)

func (s *GormStore) Current(ctx context.Context, kind string) (*domain.Document, error) {
	var d domain.Document
	err := s.DB.WithContext(ctx).Where("kind = ? AND status = ?", kind, domain.StatusPublished).
		Order("version DESC").Take(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDocumentNotFound
	}
	return &d, err
}

func (s *GormStore) Document(ctx context.Context, kind string, version int) (*domain.Document, error) {
	var d domain.Document
	err := s.DB.WithContext(ctx).Where("kind = ? AND version = ?", kind, version).Take(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDocumentNotFound
	}
	return &d, err
}

func (s *GormStore) Versions(ctx context.Context, kind string) ([]domain.Document, error) {
	out := []domain.Document{}
	return out, s.DB.WithContext(ctx).Where("kind = ?", kind).Order("version DESC").Find(&out).Error
}

func (s *GormStore) CurrentVersions(ctx context.Context) (map[string]int, error) {
	var rows []struct {
		Kind    string
		Version int
	}
	err := s.DB.WithContext(ctx).Raw(`SELECT kind, MAX(version) AS version FROM legal_documents
		WHERE status = ? GROUP BY kind`, domain.StatusPublished).Scan(&rows).Error
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Kind] = r.Version
	}
	return out, err
}

// CreateDraft : numéro suivant, pris sous verrou de la sorte (deux créations
// simultanées ne prennent pas le même numéro ; l'index unique des brouillons
// refuse le second).
func (s *GormStore) CreateDraft(ctx context.Context, d *domain.Document) error {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('legal_documents:' || ?))`, d.Kind).Error; err != nil {
			return err
		}
		var max int
		if err := tx.Raw(`SELECT COALESCE(MAX(version), 0) FROM legal_documents WHERE kind = ?`, d.Kind).Row().Scan(&max); err != nil {
			return err
		}
		d.Version = max + 1
		return tx.Create(d).Error
	})
	if isUniqueViolation(err) {
		return domain.ErrDraftExists
	}
	return err
}

func (s *GormStore) UpdateDraft(ctx context.Context, d *domain.Document) error {
	res := s.DB.WithContext(ctx).Model(&domain.Document{}).
		Where("kind = ? AND version = ? AND status = ?", d.Kind, d.Version, domain.StatusDraft).
		Updates(map[string]any{"title_fr": d.TitleFR, "title_en": d.TitleEN, "body_fr": d.BodyFR, "body_en": d.BodyEN,
			"updated_by": d.UpdatedBy, "updated_at": d.UpdatedAt})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return s.notDraft(ctx, d.Kind, d.Version)
	}
	return nil
}

func (s *GormStore) Publish(ctx context.Context, kind string, version int, by string, at time.Time) (*domain.Document, error) {
	res := s.DB.WithContext(ctx).Model(&domain.Document{}).
		Where("kind = ? AND version = ? AND status = ?", kind, version, domain.StatusDraft).
		Updates(map[string]any{"status": domain.StatusPublished, "published_by": by, "published_at": at})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, s.notDraft(ctx, kind, version)
	}
	return s.Document(ctx, kind, version)
}

func (s *GormStore) DeleteDraft(ctx context.Context, kind string, version int) error {
	res := s.DB.WithContext(ctx).Where("kind = ? AND version = ? AND status = ?", kind, version, domain.StatusDraft).
		Delete(&domain.Document{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return s.notDraft(ctx, kind, version)
	}
	return nil
}

// notDraft : rien n'a changé : version absente, ou publiée.
func (s *GormStore) notDraft(ctx context.Context, kind string, version int) error {
	if _, err := s.Document(ctx, kind, version); err != nil {
		return err
	}
	return domain.ErrNotDraft
}

func (s *GormStore) Accepted(ctx context.Context, org uuid.UUID) (map[string]domain.Acceptance, error) {
	var rows []domain.Acceptance
	err := s.DB.WithContext(ctx).Raw(`SELECT DISTINCT ON (kind) * FROM legal_acceptances
		WHERE organization_id = ? ORDER BY kind, version DESC`, org).Scan(&rows).Error
	out := make(map[string]domain.Acceptance, len(rows))
	for _, a := range rows {
		out[a.Kind] = a
	}
	return out, err
}

func (s *GormStore) Accept(ctx context.Context, a *domain.Acceptance) (*domain.Acceptance, error) {
	db := s.DB.WithContext(ctx)
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(a).Error; err != nil {
		return nil, err
	}
	var out domain.Acceptance
	err := db.Where("organization_id = ? AND kind = ? AND version = ?", a.OrganizationID, a.Kind, a.Version).Take(&out).Error
	return &out, err
}

func (s *GormStore) AcceptanceCounts(ctx context.Context, kind string) (map[int]int64, error) {
	var rows []struct {
		Version int
		N       int64
	}
	err := s.DB.WithContext(ctx).Raw(`SELECT version, COUNT(*) AS n FROM legal_acceptances WHERE kind = ? GROUP BY version`, kind).
		Scan(&rows).Error
	out := make(map[int]int64, len(rows))
	for _, r := range rows {
		out[r.Version] = r.N
	}
	return out, err
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &pg) && pg.Code == pgerr.UniqueViolation)
}
