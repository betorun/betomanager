package service

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/thekrauss/kubemanager/internal/core/country"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// RegionStore : catalogue des régions et table pays → région
// (repository.RegionStore).
type RegionStore interface {
	CountryRegions(ctx context.Context) ([]domain.CountryRegion, error)
	RegionForCountry(ctx context.Context, country string) (string, error)
	SetCountryRegion(ctx context.Context, e *domain.CountryRegion) error
	DeleteCountryRegion(ctx context.Context, country string) error
	RegionProbes(ctx context.Context) (map[string]string, error)
	RegionCatalog(ctx context.Context) ([]domain.RegionSetting, error)
	CreateRegion(ctx context.Context, e *domain.RegionSetting) error
	SaveRegion(ctx context.Context, e *domain.RegionSetting) error
	DeleteRegion(ctx context.Context, code string) error
}

// RegionInput : nouvelle région du catalogue.
type RegionInput struct {
	Code     string
	Zone     string
	Country  string
	City     string
	ProbeURL string
}

// RegionPatch : champs modifiés d'une région (nil : inchangé). Le code ne
// change jamais : clusters, projets et pays s'y réfèrent.
type RegionPatch struct {
	Zone     *string
	Country  *string
	City     *string
	ProbeURL *string
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
	catalog, err := r.catalogByCode(ctx)
	if err != nil {
		return nil, err
	}
	return domain.Suggest(rs, suggested, catalog), nil
}

// catalogByCode : régions du catalogue par code.
func (r *Registry) catalogByCode(ctx context.Context) (map[string]domain.RegionSetting, error) {
	list, err := r.Geo.RegionCatalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.RegionSetting, len(list))
	for _, e := range list {
		out[e.Code] = e
	}
	return out, nil
}

// catalogRegion : région du catalogue ; absente : ErrUnknownRegion.
func (r *Registry) catalogRegion(ctx context.Context, code string) (domain.RegionSetting, error) {
	if !domain.ValidRegionCode(code) {
		return domain.RegionSetting{}, domain.ErrInvalidRegionCode
	}
	catalog, err := r.catalogByCode(ctx)
	if err != nil {
		return domain.RegionSetting{}, err
	}
	e, ok := catalog[code]
	if !ok {
		return domain.RegionSetting{}, domain.ErrUnknownRegion
	}
	return e, nil
}

// knownRegion : la région est au catalogue.
func (r *Registry) knownRegion(ctx context.Context, region string) error {
	_, err := r.catalogRegion(ctx, region)
	return err
}

// checkClusterRegion : la région d'un cluster est vide ou au catalogue
// (sans magasin, seul le format est contrôlé, par ValidateRegistry).
func (r *Registry) checkClusterRegion(ctx context.Context, region string) error {
	if region == "" || r.Geo == nil {
		return nil
	}
	return r.knownRegion(ctx, region)
}

// CountryRegions : table pays → région (back-office), triée par pays.
func (r *Registry) CountryRegions(ctx context.Context) ([]domain.CountryRegion, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	return r.Geo.CountryRegions(ctx)
}

// SetCountryRegion : région suggérée aux clients d'un pays ; elle doit être
// au catalogue (même sans cluster encore : elle sera proposée dès qu'un
// cluster y ouvrira).
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

// RegionSettings : catalogue des régions, avec le nombre de clusters et de
// pays qui utilisent chacune. Une région portée par un cluster mais absente
// du catalogue (rien de tel n'est censé exister après 000082) y figure,
// zone, pays et ville vides.
func (r *Registry) RegionSettings(ctx context.Context) ([]domain.RegionSetting, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	catalog, err := r.catalogByCode(ctx)
	if err != nil {
		return nil, err
	}
	clusters, countries, err := r.regionUse(ctx)
	if err != nil {
		return nil, err
	}
	for code := range clusters {
		if _, ok := catalog[code]; !ok {
			catalog[code] = domain.RegionSetting{Code: code}
		}
	}
	out := make([]domain.RegionSetting, 0, len(catalog))
	for code, e := range catalog {
		e.Clusters, e.Countries = clusters[code], countries[code]
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b domain.RegionSetting) int { return strings.Compare(a.Code, b.Code) })
	return out, nil
}

// regionUse : nombre de clusters (tous états) et de pays par région.
func (r *Registry) regionUse(ctx context.Context) (clusters, countries map[string]int, err error) {
	cs, err := r.Store.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	clusters = map[string]int{}
	for _, c := range cs {
		if c.Region != "" {
			clusters[c.Region]++
		}
	}
	crs, err := r.Geo.CountryRegions(ctx)
	if err != nil {
		return nil, nil, err
	}
	countries = map[string]int{}
	for _, e := range crs {
		countries[e.Region]++
	}
	return clusters, countries, nil
}

// regionPlace : zone, pays et ville contrôlés.
func regionPlace(zone, rawCountry, rawCity string) (string, string, string, error) {
	zone = strings.TrimSpace(zone)
	if !domain.ValidRegionZone(zone) {
		return "", "", "", domain.ErrInvalidRegionZone
	}
	cc, err := country.Parse(rawCountry)
	if err != nil || cc == "" {
		return "", "", "", country.ErrInvalid
	}
	city := domain.NormalizeCity(rawCity)
	if city == "" {
		return "", "", "", domain.ErrInvalidRegionCity
	}
	return zone, cc, city, nil
}

// CreateRegion : nouvelle région du catalogue (zone, pays et ville
// obligatoires, adresse de mesure facultative).
func (r *Registry) CreateRegion(ctx context.Context, in RegionInput, by string) (*domain.RegionSetting, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	code := strings.TrimSpace(in.Code)
	if !domain.ValidRegionCode(code) {
		return nil, domain.ErrInvalidRegionCode
	}
	zone, cc, city, err := regionPlace(in.Zone, in.Country, in.City)
	if err != nil {
		return nil, err
	}
	probe, err := domain.NormalizeProbeURL(in.ProbeURL)
	if err != nil {
		return nil, err
	}
	e := &domain.RegionSetting{Code: code, Zone: zone, Country: cc, City: city, ProbeURL: probe, UpdatedBy: by}
	if err := r.Geo.CreateRegion(ctx, e); err != nil {
		return nil, err
	}
	r.invalidateProbeOrigins()
	return e, nil
}

// UpdateRegion : zone, pays, ville ou adresse de mesure d'une région du
// catalogue (adresse "" : retirée). Les champs fournis sont contrôlés comme
// à la création ; ceux d'une région encore à compléter restent vides tant
// qu'ils ne sont pas fournis.
func (r *Registry) UpdateRegion(ctx context.Context, code string, in RegionPatch, by string) (*domain.RegionSetting, error) {
	if r.Geo == nil {
		return nil, ErrNoRegionStore
	}
	e, err := r.catalogRegion(ctx, strings.TrimSpace(code))
	if err != nil {
		return nil, err
	}
	if in.Zone != nil || in.Country != nil || in.City != nil {
		zone, cc, city := e.Zone, e.Country, e.City
		if in.Zone != nil {
			zone = *in.Zone
		}
		if in.Country != nil {
			cc = *in.Country
		}
		if in.City != nil {
			city = *in.City
		}
		if e.Zone, e.Country, e.City, err = regionPlace(zone, cc, city); err != nil {
			return nil, err
		}
	}
	if in.ProbeURL != nil {
		if e.ProbeURL, err = domain.NormalizeProbeURL(*in.ProbeURL); err != nil {
			return nil, err
		}
	}
	e.UpdatedBy = by
	if err := r.Geo.SaveRegion(ctx, &e); err != nil {
		return nil, err
	}
	r.invalidateProbeOrigins()
	return &e, nil
}

// DeleteRegion : retire une région du catalogue ; refusé tant qu'un cluster
// la porte ou qu'un pays la suggère.
func (r *Registry) DeleteRegion(ctx context.Context, code string) error {
	if r.Geo == nil {
		return ErrNoRegionStore
	}
	code = strings.TrimSpace(code)
	if err := r.knownRegion(ctx, code); err != nil {
		return err
	}
	clusters, countries, err := r.regionUse(ctx)
	if err != nil {
		return err
	}
	if clusters[code] > 0 || countries[code] > 0 {
		return domain.ErrRegionInUse
	}
	if err := r.Geo.DeleteRegion(ctx, code); err != nil {
		return err
	}
	r.invalidateProbeOrigins()
	return nil
}

// probeOriginsTTL : durée de vie du cache de ProbeOrigins (route publique,
// lue à chaque chargement de la console).
const probeOriginsTTL = time.Minute

// probeCache : dernières origines calculées. gen change à chaque écriture qui
// touche aux régions proposées ou à leurs adresses : un calcul lancé avant
// n'est ni gardé ni partagé avec les appels suivants.
type probeCache struct {
	mu     sync.Mutex
	gen    uint64
	at     time.Time
	vals   []string
	flight singleflight.Group
}

// invalidateProbeOrigins : le prochain ProbeOrigins relit la base.
func (r *Registry) invalidateProbeOrigins() {
	r.probes.mu.Lock()
	r.probes.gen++
	r.probes.vals = nil
	r.probes.mu.Unlock()
}

// ProbeOrigins : origines (https://hôte[:port]) des adresses de mesure des
// régions proposées (celles de GET /regions), sans doublon, triées. La console
// les autorise dans sa politique de contenu (connect-src) pour mesurer le
// temps de réponse depuis le navigateur. Rien d'autre n'en sort : ni code de
// région, ni chemin. Gardées en mémoire probeOriginsTTL (par instance de
// l'API), calcul partagé entre appels simultanés.
func (r *Registry) ProbeOrigins(ctx context.Context) ([]string, error) {
	if r.Geo == nil {
		return []string{}, nil
	}
	c := &r.probes
	c.mu.Lock()
	if c.vals != nil && r.now().Sub(c.at) < probeOriginsTTL {
		out := slices.Clone(c.vals)
		c.mu.Unlock()
		return out, nil
	}
	gen := c.gen
	c.mu.Unlock()
	v, err, _ := c.flight.Do(strconv.FormatUint(gen, 10), func() (any, error) {
		// Sans l'annulation de l'appelant : le calcul sert aussi aux autres.
		out, err := r.loadProbeOrigins(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.gen == gen {
			c.vals, c.at = out, r.now()
		}
		c.mu.Unlock()
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Clone(v.([]string)), nil
}

// loadProbeOrigins : calcul de ProbeOrigins. Chaque adresse repasse le
// contrôle de SetRegionProbe : une ligne invalide en base est ignorée.
func (r *Registry) loadProbeOrigins(ctx context.Context) ([]string, error) {
	cs, err := r.candidates(ctx)
	if err != nil {
		return nil, err
	}
	probes, err := r.Geo.RegionProbes(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	for _, reg := range domain.Regions(cs) {
		raw, ok := probes[reg.Code]
		if !ok {
			continue
		}
		o, err := domain.ProbeOrigin(raw)
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warnw("invalid probe address skipped", "region", reg.Code)
			}
			continue
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	slices.Sort(out)
	return out, nil
}
