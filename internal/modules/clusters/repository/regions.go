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

// SetRegionProbe : adresse de mesure de la région ("" : retirée).
func (s *RegionStore) SetRegionProbe(ctx context.Context, e *domain.RegionSetting) error {
	e.UpdatedAt = time.Now().UTC()
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "region"}},
		DoUpdates: clause.AssignmentColumns([]string{"probe_url", "updated_at", "updated_by"}),
	}).Create(e).Error
}
