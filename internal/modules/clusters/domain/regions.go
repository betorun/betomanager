package domain

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Région suggérée selon le pays du client : table pays → région tenue par
// l'équipe dans le back-office (jamais en dur), adresse de mesure de latence
// de chaque région.

var (
	// ErrUnknownRegion : aucun cluster enregistré ne porte cette région.
	ErrUnknownRegion = errors.New("unknown region: no registered cluster is in this region")
	// ErrInvalidProbeURL : adresse de mesure qui n'est pas une URL https.
	ErrInvalidProbeURL = errors.New("invalid probe address: an https URL is required")
)

// maxProbeURL : longueur maximale d'une adresse de mesure.
const maxProbeURL = 512

// CountryRegion : région suggérée aux clients d'un pays.
type CountryRegion struct {
	Country   string    `gorm:"primaryKey;type:varchar(2)" json:"country" description:"Pays (code ISO 3166-1 à deux lettres)"`
	Region    string    `gorm:"type:varchar(32);not null" json:"region" description:"Code de la région suggérée"`
	UpdatedAt time.Time `json:"-"`
	UpdatedBy string    `gorm:"not null;default:''" json:"-"`
}

func (CountryRegion) TableName() string { return "country_regions" }

// RegionSetting : réglages d'une région (code des clusters).
type RegionSetting struct {
	Code      string    `gorm:"column:region;primaryKey;type:varchar(32)" json:"code" description:"Code de la région"`
	ProbeURL  string    `gorm:"not null;default:''" json:"probe_url" description:"Adresse https de mesure du temps de réponse ; vide si non réglée"`
	UpdatedAt time.Time `json:"-"`
	UpdatedBy string    `gorm:"not null;default:''" json:"-"`
}

func (RegionSetting) TableName() string { return "region_settings" }

// NormalizeProbeURL : adresse https absolue sans identifiants ; "" retire
// l'adresse.
func NormalizeProbeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxProbeURL || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", ErrInvalidProbeURL
	}
	return u.String(), nil
}

// Suggest : région suggérée (suggested, "" : aucune) et adresses de mesure
// posées sur les régions ; la suggérée passe en tête, l'ordre reste sinon.
func Suggest(rs []Region, suggested string, probes map[string]string) []Region {
	for i := range rs {
		rs[i].Suggested = suggested != "" && rs[i].Code == suggested
		rs[i].ProbeURL = probes[rs[i].Code]
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Suggested && !rs[j].Suggested })
	return rs
}

// KnownRegions : régions des clusters enregistrés (tous états), triées.
func KnownRegions(cs []Cluster) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if c.Region != "" && !seen[c.Region] {
			seen[c.Region] = true
			out = append(out, c.Region)
		}
	}
	sort.Strings(out)
	return out
}
