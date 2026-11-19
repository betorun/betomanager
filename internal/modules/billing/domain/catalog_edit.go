package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidCatalog : grille refusée (le message dit pourquoi).
var ErrInvalidCatalog = errors.New("invalid price catalog")

// CatalogError : grille refusée, avec la raison.
type CatalogError struct{ Reason string }

func (e *CatalogError) Error() string { return "invalid price catalog: " + e.Reason }
func (e *CatalogError) Unwrap() error { return ErrInvalidCatalog }

func invalid(format string, args ...any) error {
	return &CatalogError{Reason: fmt.Sprintf(format, args...)}
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	sizeIDRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
)

// maxSizes : tailles proposées au plus.
const maxSizes = 12

// NormalizeCurrency : code ISO 4217 en majuscules, ou "".
func NormalizeCurrency(raw string) string {
	c := strings.ToUpper(strings.TrimSpace(raw))
	if !currencyRe.MatchString(c) {
		return ""
	}
	return c
}

// CatalogUpdate : grille saisie par l'équipe (montants en unités de la
// monnaie, par mois ; une minute pour les builds).
type CatalogUpdate struct {
	Currency        string     `path:"currency" json:"-" description:"Monnaie (code ISO 4217, ex. XAF)"`
	Sizes           []SizeView `json:"sizes" description:"Tailles de conteneur, de la plus petite à la plus grande"`
	DatabaseMonthly int64      `json:"database_monthly" description:"Prix mensuel d'une instance de base"`
	DiskGiMonthly   int64      `json:"disk_gi_monthly" description:"Prix mensuel d'un Go de disque (base ou conteneur)"`
	BuildMinute     int64      `json:"build_minute" description:"Prix d'une minute de build au-delà de l'offre gratuite"`
	SiteDomain      int64      `json:"site_domain_monthly" description:"Prix mensuel d'un domaine personnalisé de site (0 : gratuit)"`
	Free            FreeView   `json:"free_tier" description:"Offre gratuite par organisation et par mois"`
}

// CatalogFromUpdate : grille validée. Les tailles doivent croître (CPU,
// mémoire et prix) : un conteneur est classé dans la plus petite qui le couvre.
func CatalogFromUpdate(u CatalogUpdate) (Catalog, error) {
	currency := NormalizeCurrency(u.Currency)
	if currency == "" {
		return Catalog{}, invalid("currency must be an ISO 4217 code such as XAF")
	}
	if len(u.Sizes) == 0 || len(u.Sizes) > maxSizes {
		return Catalog{}, invalid("between 1 and %d container sizes", maxSizes)
	}
	c := Catalog{Currency: currency}
	seen := map[string]bool{}
	for i, s := range u.Sizes {
		id := strings.TrimSpace(s.ID)
		switch {
		case !sizeIDRe.MatchString(id) || id == SizeCustom:
			return Catalog{}, invalid("size %d: id must be lowercase letters, digits or dashes (not %q)", i+1, SizeCustom)
		case seen[id]:
			return Catalog{}, invalid("size %q appears twice", id)
		case s.CPUMilli <= 0 || s.MemMi <= 0 || s.Monthly <= 0:
			return Catalog{}, invalid("size %q: CPU, memory and price must be positive", id)
		}
		if i > 0 {
			prev := u.Sizes[i-1]
			if s.CPUMilli < prev.CPUMilli || s.MemMi < prev.MemMi || s.Monthly < prev.Monthly ||
				(s.CPUMilli == prev.CPUMilli && s.MemMi == prev.MemMi) {
				return Catalog{}, invalid("size %q must be larger than %q (CPU, memory and price)", id, prev.ID)
			}
		}
		seen[id] = true
		c.Sizes = append(c.Sizes, Size{ID: id, CPUMilli: s.CPUMilli, MemMi: s.MemMi, Monthly: FromUnits(s.Monthly)})
	}
	for name, v := range map[string]int64{
		"database_monthly": u.DatabaseMonthly, "disk_gi_monthly": u.DiskGiMonthly, "build_minute": u.BuildMinute,
		"site_domain_monthly": u.SiteDomain, "free_tier.compute": u.Free.Compute, "free_tier.database": u.Free.Database,
		"free_tier.build_minutes": u.Free.BuildMinutes,
	} {
		if v < 0 {
			return Catalog{}, invalid("%s cannot be negative", name)
		}
	}
	c.DatabaseMonthly, c.DiskGiMonthly = FromUnits(u.DatabaseMonthly), FromUnits(u.DiskGiMonthly)
	c.BuildMinute, c.SiteDomain = FromUnits(u.BuildMinute), FromUnits(u.SiteDomain)
	c.Free = FreeTier{Compute: FromUnits(u.Free.Compute), Database: FromUnits(u.Free.Database), BuildMinutes: u.Free.BuildMinutes}
	return c, nil
}

// Update : la grille sous forme de saisie (stockage en base, en unités).
func (c Catalog) Update() CatalogUpdate {
	v := c.View()
	return CatalogUpdate{Currency: c.Currency, Sizes: v.Sizes, DatabaseMonthly: v.DatabaseMonthly,
		DiskGiMonthly: v.DiskGiMonthly, BuildMinute: v.BuildMinute, SiteDomain: v.SiteDomain, Free: v.Free}
}

// CatalogVersionView : une version de grille (administration).
type CatalogVersionView struct {
	CatalogView
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

// CatalogsResponse : grilles en vigueur, une par monnaie.
type CatalogsResponse struct {
	Catalogs []CatalogVersionView `json:"catalogs"`
}
