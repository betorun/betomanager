package service

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/thekrauss/kubemanager/internal/core/country"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// RegionStore : table pays → région et réglages des régions
// (repository.RegionStore).
type RegionStore interface {
	CountryRegions(ctx context.Context) ([]domain.CountryRegion, error)
	RegionForCountry(ctx context.Context, country string) (string, error)
	SetCountryRegion(ctx context.Context, e *domain.CountryRegion) error
	DeleteCountryRegion(ctx context.Context, country string) error
	RegionProbes(ctx context.Context) (map[string]string, error)
	SetRegionProbe(ctx context.Context, e *domain.RegionSetting) error
}

// ErrNoRegionStore : table pays → région indisponible (déploiement sans base).
var ErrNoRegionStore = errors.New("region settings unavailable")

// Regions : régions proposées à la création d'un projet ; celle associée au
// pays du client (cc, "" : inconnu) est suggérée et passe en tête.
func (r *Registry) Regions(ctx context.Context, cc string) ([]domain.Region, error) {
	cs, err := r.candidates(ctx)
	if err != nil {
		return nil, err
	}
	rs := domain.Regions(cs)
	if r.Geo == nil {
		return rs, nil
	}
	suggested := ""
	if cc != "" {
		if suggested, err = r.Geo.RegionForCountry(ctx, cc); err != nil {
			return nil, err
		}
	}
	probes, err := r.Geo.RegionProbes(ctx)
	if err != nil {
		return nil, err
	}
	return domain.Suggest(rs, suggested, probes), nil
}

// knownRegion : la région est celle d'au moins un cluster enregistré.
func (r *Registry) knownRegion(ctx context.Context, region string) error {
	if !domain.ValidRegionCode(region) {
		return domain.ErrInvalidRegionCode
	}
	cs, err := r.Store.List(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(domain.KnownRegions(cs), region) {
		return domain.ErrUnknownRegion
	}
	return nil
}

// CountryRegions : table pays → région (back-office), triée par pays.
func (r *Registry) CountryRegions(ctx context.Context) ([]domain.CountryRegion, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	return r.Geo.CountryRegions(ctx)
}

// SetCountryRegion : région suggérée aux clients d'un pays ; elle doit être
// celle d'au moins un cluster enregistré.
func (r *Registry) SetCountryRegion(ctx context.Context, rawCountry, region, by string) (*domain.CountryRegion, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	cc, err := country.Parse(rawCountry)
	if err != nil || cc == "" {
		return nil, country.ErrInvalid
	}
	region = strings.TrimSpace(region)
	if err := r.knownRegion(ctx, region); err != nil {
		return nil, err
	}
	e := &domain.CountryRegion{Country: cc, Region: region, UpdatedBy: by}
	if err := r.Geo.SetCountryRegion(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// DeleteCountryRegion : plus de région suggérée pour ce pays.
func (r *Registry) DeleteCountryRegion(ctx context.Context, rawCountry string) error {
	if r.Geo == nil {
		return ErrNoRegionStore
	}
	cc, err := country.Parse(rawCountry)
	if err != nil || cc == "" {
		return country.ErrInvalid
	}
	return r.Geo.DeleteCountryRegion(ctx, cc)
}

// RegionSettings : régions connues des clusters et leur adresse de mesure.
func (r *Registry) RegionSettings(ctx context.Context) ([]domain.RegionSetting, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	cs, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	probes, err := r.Geo.RegionProbes(ctx)
	if err != nil {
		return nil, err
	}
	codes := domain.KnownRegions(cs)
	out := make([]domain.RegionSetting, 0, len(codes))
	for _, code := range codes {
		out = append(out, domain.RegionSetting{Code: code, ProbeURL: probes[code]})
	}
	return out, nil
}

// SetRegionProbe : adresse https de mesure du temps de réponse de la région
// ("" : retirée).
func (r *Registry) SetRegionProbe(ctx context.Context, code, rawURL, by string) (*domain.RegionSetting, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	code = strings.TrimSpace(code)
	if err := r.knownRegion(ctx, code); err != nil {
		return nil, err
	}
	u, err := domain.NormalizeProbeURL(rawURL)
	if err != nil {
		return nil, err
	}
	e := &domain.RegionSetting{Code: code, ProbeURL: u, UpdatedBy: by}
	if err := r.Geo.SetRegionProbe(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// ProbeOrigins : origines (https://hôte[:port]) des adresses de mesure des
// régions, sans doublon, triées. La console les autorise dans sa politique de
// contenu (connect-src) pour mesurer le temps de réponse depuis le navigateur.
// Rien d'autre n'en sort : ni code de région, ni chemin.
func (r *Registry) ProbeOrigins(ctx context.Context) ([]string, error) {
	out := []string{}
	if r.Geo == nil {
		return out, nil
	}
	probes, err := r.Geo.RegionProbes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, raw := range probes {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			continue
		}
		o := "https://" + strings.ToLower(u.Host)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	slices.Sort(out)
	return out, nil
}
