package repository

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// RegionStore : table pays → région et réglages des régions (Postgres).
type RegionStore struct{ DB *gorm.DB }

// CountryRegions : toute la table, triée par pays.
func (s *RegionStore) CountryRegions(ctx context.Context) ([]domain.CountryRegion, error) {
	out := []domain.CountryRegion{}
	return out, s.DB.WithContext(ctx).Order("country").Find(&out).Error
}

// RegionForCountry : région associée au pays ("" : aucune).
func (s *RegionStore) RegionForCountry(ctx context.Context, country string) (string, error) {
	var region string
	err := s.DB.WithContext(ctx).Raw(`SELECT region FROM country_regions WHERE country = ?`, country).Scan(&region).Error
	return region, err
}

// SetCountryRegion : crée ou remplace l'entrée du pays.
func (s *RegionStore) SetCountryRegion(ctx context.Context, e *domain.CountryRegion) error {
	e.UpdatedAt = time.Now().UTC()
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "country"}},
		DoUpdates: clause.AssignmentColumns([]string{"region", "updated_at", "updated_by"}),
	}).Create(e).Error
}

// DeleteCountryRegion : retire l'entrée du pays (absente : rien).
func (s *RegionStore) DeleteCountryRegion(ctx context.Context, country string) error {
	return s.DB.WithContext(ctx).Where("country = ?", country).Delete(&domain.CountryRegion{}).Error
}

// RegionProbes : adresse de mesure de chaque région qui en a une.
func (s *RegionStore) RegionProbes(ctx context.Context) (map[string]string, error) {
	var rows []domain.RegionSetting
	if err := s.DB.WithContext(ctx).Where("probe_url <> ''").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Code] = r.ProbeURL
	}
	return out, nil
}

// RegionCatalog : toutes les régions du catalogue, triées par code.
func (s *RegionStore) RegionCatalog(ctx context.Context) ([]domain.RegionSetting, error) {
	out := []domain.RegionSetting{}
	return out, s.DB.WithContext(ctx).Order("region").Find(&out).Error
}

// CreateRegion : nouvelle région ; code déjà pris : ErrRegionExists.
func (s *RegionStore) CreateRegion(ctx context.Context, e *domain.RegionSetting) error {
	now := time.Now().UTC()
	e.CreatedAt, e.UpdatedAt = now, now
	res := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(e)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrRegionExists
	}
	return nil
}

// SaveRegion : zone, pays, ville et adresse de mesure d'une région du
// catalogue ; absente : ErrUnknownRegion.
func (s *RegionStore) SaveRegion(ctx context.Context, e *domain.RegionSetting) error {
	e.UpdatedAt = time.Now().UTC()
	res := s.DB.WithContext(ctx).Model(&domain.RegionSetting{}).Where("region = ?", e.Code).Updates(map[string]any{
		"zone": e.Zone, "country": e.Country, "city": e.City, "probe_url": e.ProbeURL,
		"updated_at": e.UpdatedAt, "updated_by": e.UpdatedBy,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrUnknownRegion
	}
	return nil
}

// DeleteRegion : retire la région du catalogue (absente : rien). Le service
// vérifie avant qu'aucun cluster ni aucun pays ne l'utilise.
func (s *RegionStore) DeleteRegion(ctx context.Context, code string) error {
	return s.DB.WithContext(ctx).Where("region = ?", code).Delete(&domain.RegionSetting{}).Error
}
