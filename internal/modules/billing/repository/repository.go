// Package repository : tables de la facturation (billing_usage,
// billing_accounts). Seul ce module y écrit.
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Store : usage et comptes.
type Store interface {
	// RecordUsage : enregistre les lignes ; une ligne existante (même compteur,
	// ressource et heure) garde le plus haut montant. Idempotent.
	RecordUsage(ctx context.Context, lines []domain.Usage) error
	// Totals : usage cumulé par ressource d'une organisation sur [from, to).
	Totals(ctx context.Context, org uuid.UUID, from, to time.Time) ([]domain.Total, error)
	// HourAmounts : montant de chaque ressource horaire pour une heure.
	HourAmounts(ctx context.Context, org uuid.UUID, hour time.Time) (map[uuid.UUID]map[domain.Meter]domain.Milli, error)
	// Currency : monnaie du compte de l'organisation (défaut sans compte).
	Currency(ctx context.Context, org uuid.UUID) (string, error)
	// ActiveCatalog : grille en vigueur d'une monnaie (domain.ErrUnknownCurrency
	// si aucune).
	ActiveCatalog(ctx context.Context, currency string) (domain.Catalog, error)
	// ActiveCatalogs : grilles en vigueur, une par monnaie.
	ActiveCatalogs(ctx context.Context) ([]domain.CatalogVersionView, error)
	// SaveCatalog : nouvelle version de la grille de sa monnaie.
	SaveCatalog(ctx context.Context, c domain.Catalog, by string) (domain.Catalog, error)
}

type gormStore struct{ db *gorm.DB }

// New : stockage en base.
func New(db *gorm.DB) Store { return &gormStore{db: db} }

const recordBatch = 500

func (s *gormStore) RecordUsage(ctx context.Context, lines []domain.Usage) error {
	if len(lines) == 0 {
		return nil
	}
	now := time.Now()
	for i := range lines {
		lines[i].CreatedAt, lines[i].UpdatedAt = now, now
	}
	// Le plus haut montant de l'heure l'emporte, avec sa taille et sa quantité.
	higher := "excluded.amount > billing_usage.amount"
	keep := func(col string) clause.Expr {
		return gorm.Expr("CASE WHEN " + higher + " THEN excluded." + col + " ELSE billing_usage." + col + " END")
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "meter"}, {Name: "resource_id"}, {Name: "hour"}},
		DoUpdates: clause.Assignments(map[string]any{
			"amount":          gorm.Expr("GREATEST(billing_usage.amount, excluded.amount)"),
			"size":            keep("size"),
			"quantity":        keep("quantity"),
			"unit_price":      keep("unit_price"),
			"catalog_version": keep("catalog_version"),
			"resource_name":   gorm.Expr("excluded.resource_name"),
			"updated_at":      gorm.Expr("excluded.updated_at"),
		}),
	}).CreateInBatches(lines, recordBatch).Error
}

type totalRow struct {
	ProjectID    uuid.UUID
	Meter        string
	ResourceID   uuid.UUID
	ResourceName string
	Size         string
	Quantity     int64
	Hours        int64
	Sum          int64
	Peak         int64
}

func (s *gormStore) Totals(ctx context.Context, org uuid.UUID, from, to time.Time) ([]domain.Total, error) {
	var rows []totalRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT project_id, meter, resource_id,
		       (array_agg(resource_name ORDER BY hour DESC))[1] AS resource_name,
		       (array_agg(size ORDER BY hour DESC))[1]          AS size,
		       (array_agg(quantity ORDER BY hour DESC))[1]      AS quantity,
		       COUNT(*)    AS hours,
		       SUM(amount) AS sum,
		       MAX(amount) AS peak
		FROM billing_usage
		WHERE organization_id = ? AND hour >= ? AND hour < ?
		GROUP BY project_id, meter, resource_id`, org, from, to).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]domain.Total, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Total{ProjectID: r.ProjectID, Meter: domain.Meter(r.Meter), ResourceID: r.ResourceID,
			ResourceName: r.ResourceName, Size: r.Size, Quantity: r.Quantity, Hours: r.Hours,
			Sum: domain.Milli(r.Sum), Peak: domain.Milli(r.Peak)})
	}
	return out, nil
}

func (s *gormStore) HourAmounts(ctx context.Context, org uuid.UUID, hour time.Time) (map[uuid.UUID]map[domain.Meter]domain.Milli, error) {
	var rows []struct {
		ResourceID uuid.UUID
		Meter      string
		Amount     int64
	}
	err := s.db.WithContext(ctx).Raw(`SELECT resource_id, meter, amount FROM billing_usage
		WHERE organization_id = ? AND hour = ? AND meter <> ?`, org, hour, domain.MeterBuildMinutes).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]map[domain.Meter]domain.Milli{}
	for _, r := range rows {
		if out[r.ResourceID] == nil {
			out[r.ResourceID] = map[domain.Meter]domain.Milli{}
		}
		out[r.ResourceID][domain.Meter(r.Meter)] = domain.Milli(r.Amount)
	}
	return out, nil
}

func (s *gormStore) Currency(ctx context.Context, org uuid.UUID) (string, error) {
	var currency string
	err := s.db.WithContext(ctx).Raw(`SELECT currency FROM billing_accounts WHERE organization_id = ?`, org).
		Row().Scan(&currency)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultCurrency, nil
	}
	return currency, err
}

// ── Grilles de prix ─────────────────────────────────────────────────────

type catalogRow struct {
	Currency  string
	Version   int
	Data      []byte
	CreatedBy string
	CreatedAt time.Time
}

func (r catalogRow) catalog() (domain.Catalog, error) {
	var u domain.CatalogUpdate
	if err := json.Unmarshal(r.Data, &u); err != nil {
		return domain.Catalog{}, fmt.Errorf("billing catalog %s v%d: %w", r.Currency, r.Version, err)
	}
	u.Currency = r.Currency
	c, err := domain.CatalogFromUpdate(u)
	if err != nil {
		return domain.Catalog{}, fmt.Errorf("billing catalog %s v%d: %w", r.Currency, r.Version, err)
	}
	c.Version = strconv.Itoa(r.Version)
	return c, nil
}

const activeCatalogs = `SELECT DISTINCT ON (currency) currency, version, data, created_by, created_at
	FROM billing_catalogs %s ORDER BY currency, version DESC`

func (s *gormStore) ActiveCatalog(ctx context.Context, currency string) (domain.Catalog, error) {
	var rows []catalogRow
	if err := s.db.WithContext(ctx).Raw(fmt.Sprintf(activeCatalogs, "WHERE currency = ?"), currency).Scan(&rows).Error; err != nil {
		return domain.Catalog{}, err
	}
	if len(rows) == 0 {
		return domain.Catalog{}, domain.ErrUnknownCurrency
	}
	return rows[0].catalog()
}

func (s *gormStore) ActiveCatalogs(ctx context.Context) ([]domain.CatalogVersionView, error) {
	var rows []catalogRow
	if err := s.db.WithContext(ctx).Raw(fmt.Sprintf(activeCatalogs, "")).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.CatalogVersionView, 0, len(rows))
	for _, r := range rows {
		c, err := r.catalog()
		if err != nil {
			return nil, err
		}
		out = append(out, domain.CatalogVersionView{CatalogView: c.View(), CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

func (s *gormStore) SaveCatalog(ctx context.Context, c domain.Catalog, by string) (domain.Catalog, error) {
	data, err := json.Marshal(c.Update())
	if err != nil {
		return domain.Catalog{}, err
	}
	// Version suivante calculée dans l'insertion : deux enregistrements
	// simultanés se heurtent à la clé primaire, aucun n'en écrase un autre.
	var version int
	err = s.db.WithContext(ctx).Raw(`
		INSERT INTO billing_catalogs (currency, version, data, created_by)
		SELECT ?, COALESCE(MAX(version), 0) + 1, ?::jsonb, ? FROM billing_catalogs WHERE currency = ?
		RETURNING version`, c.Currency, string(data), by, c.Currency).Row().Scan(&version)
	if err != nil {
		return domain.Catalog{}, err
	}
	c.Version = strconv.Itoa(version)
	return c, nil
}
