package domain

import (
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Catalogue des régions (ADR 0018), tenu par l'équipe dans le back-office :
// code, zone, pays et ville de chaque région (le libellé « Afrique centrale ·
// Brazzaville (Congo) » se construit dans la console, sans traduction par
// région), adresse de mesure de latence. Table pays → région suggérée.

var (
	// ErrUnknownRegion : la région n'est pas au catalogue.
	ErrUnknownRegion = errors.New("unknown region: add it to the region catalog first")
	// ErrInvalidProbeURL : adresse de mesure qui n'est pas une URL https.
	ErrInvalidProbeURL = errors.New("invalid probe address: an https URL is required")
	// ErrRegionExists : code déjà au catalogue.
	ErrRegionExists = errors.New("a region with this code already exists")
	// ErrRegionInUse : région portée par un cluster ou associée à un pays.
	ErrRegionInUse = errors.New("this region is used by a cluster or a country: change them first")
	// ErrInvalidRegionZone : zone hors de la liste RegionZones.
	ErrInvalidRegionZone = errors.New("invalid region zone")
	// ErrInvalidRegionCity : ville vide, trop longue ou avec des caractères refusés.
	ErrInvalidRegionCity = errors.New("invalid city: 1 to 64 letters, spaces, dots, dashes or apostrophes")
)

// maxProbeURL : longueur maximale d'une adresse de mesure.
const maxProbeURL = 512

// maxCity : longueur maximale d'un nom de ville (en caractères).
const maxCity = 64

// RegionZones : zones géographiques d'une région. La console traduit chaque
// zone ; en ajouter une demande aussi son libellé dans la console.
var RegionZones = []string{"af-west", "af-central", "af-east", "af-north", "af-south", "eu-west", "eu-central", "eu-south"}

// ValidRegionZone : zone connue.
func ValidRegionZone(z string) bool { return slices.Contains(RegionZones, z) }

// NormalizeCity : nom de ville sans espaces superflus ; "" si refusé.
// Lettres (accents compris), espaces, points, tirets et apostrophes.
func NormalizeCity(raw string) string {
	c := strings.Join(strings.Fields(raw), " ")
	if c == "" || utf8.RuneCountInString(c) > maxCity {
		return ""
	}
	for _, r := range c {
		if !unicode.IsLetter(r) && r != ' ' && r != '.' && r != '-' && r != '\'' && r != '’' {
			return ""
		}
	}
	return c
}

// CountryRegion : région suggérée aux clients d'un pays.
type CountryRegion struct {
	Country   string    `gorm:"primaryKey;type:varchar(2)" json:"country" description:"Pays (code ISO 3166-1 à deux lettres)"`
	Region    string    `gorm:"type:varchar(32);not null" json:"region" description:"Code de la région suggérée"`
	UpdatedAt time.Time `json:"-"`
	UpdatedBy string    `gorm:"not null;default:''" json:"-"`
}

func (CountryRegion) TableName() string { return "country_regions" }

// RegionSetting : une région du catalogue. Zone, pays et ville sont vides
// pour une région reprise d'un cluster avant le catalogue (à compléter).
type RegionSetting struct {
	Code     string `gorm:"column:region;primaryKey;type:varchar(32)" json:"code" description:"Code de la région"`
	Zone     string `gorm:"not null;default:''" json:"zone" description:"Zone géographique (af-west, af-central, af-east, af-north, af-south, eu-west, eu-central, eu-south) ; vide : à compléter"`
	Country  string `gorm:"type:varchar(2);not null;default:''" json:"country" description:"Pays de la région (ISO 3166-1 à deux lettres) ; vide : à compléter"`
	City     string `gorm:"not null;default:''" json:"city" description:"Ville de la région ; vide : à compléter"`
	ProbeURL string `gorm:"not null;default:''" json:"probe_url" description:"Adresse https de mesure du temps de réponse ; vide si non réglée"`
	// Clusters, Countries : clusters de la région et pays qui la suggèrent
	// (calculés ; une région utilisée ne se supprime pas).
	Clusters  int       `gorm:"-" json:"clusters" description:"Clusters enregistrés dans cette région"`
	Countries int       `gorm:"-" json:"countries" description:"Pays pour lesquels cette région est suggérée"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
	UpdatedBy string    `gorm:"not null;default:''" json:"-"`
}

func (RegionSetting) TableName() string { return "region_settings" }

// NormalizeProbeURL : adresse https absolue sans identifiants ni fragment ;
// "" retire l'adresse. L'hôte est un nom DNS (lettres, chiffres, tirets ; un
// nom internationalisé passe en punycode) ou une adresse IP littérale, le
// port est vide ou numérique (1 à 65535). Rendue sous forme normalisée (hôte
// en minuscules) : son origine part telle quelle dans la politique de contenu
// de la console, aucun caractère de cette syntaxe (; , * ' ") ne doit passer.
func NormalizeProbeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := parseProbeURL(raw)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// ProbeOrigin : origine (https://hôte[:port]) d'une adresse de mesure, après
// le même contrôle que NormalizeProbeURL.
func ProbeOrigin(raw string) (string, error) {
	u, err := parseProbeURL(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return "https://" + u.Host, nil
}

// parseProbeURL : adresse contrôlée, hôte et port normalisés.
func parseProbeURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxProbeURL {
		return nil, ErrInvalidProbeURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, ErrInvalidProbeURL
	}
	// Autorité brute (entre « https:// » et le chemin) : aucun échappement %,
	// url.Parse les décoderait dans l'hôte (%3B → ;, %25 → zone IPv6).
	authority := raw[len("https://"):]
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if strings.Contains(authority, "%") {
		return nil, ErrInvalidProbeURL
	}
	host, ok := probeHost(u.Host)
	if !ok {
		return nil, ErrInvalidProbeURL
	}
	u.Host = host
	return u, nil
}

// probeHost : « hôte[:port] » normalisé ; false : hôte ou port refusé.
func probeHost(hostport string) (string, bool) {
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.Index(hostport, "]")
		if end < 0 {
			return "", false
		}
		host, port = hostport[1:end], hostport[end+1:]
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || a.Zone() != "" {
			return "", false
		}
		host = "[" + a.String() + "]"
	} else {
		if i := strings.LastIndex(hostport, ":"); i >= 0 {
			host, port = hostport[:i], hostport[i:]
		}
		if a, err := netip.ParseAddr(host); err == nil {
			if !a.Is4() {
				return "", false
			}
			host = a.String()
		} else if host = probeDNSName(host); host == "" {
			return "", false
		}
	}
	if port != "" {
		n, ok := probePort(port)
		if !ok {
			return "", false
		}
		return host + ":" + strconv.Itoa(n), true
	}
	return host, true
}

// probePort : « :n », n entre 1 et 65535 (« : » seul refusé).
func probePort(p string) (int, bool) {
	digits, ok := strings.CutPrefix(p, ":")
	if !ok || digits == "" || len(digits) > 5 {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n >= 1 && n <= 65535
}

// probeDNSName : nom DNS en ASCII minuscule (IDN → punycode, profil Lookup),
// au moins deux labels [a-z0-9]([a-z0-9-]*[a-z0-9])?, 253 caractères au plus,
// dernier label non entièrement numérique (une IPv4 invalide n'en est pas un) ;
// "" : refusé.
func probeDNSName(h string) string {
	a, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return ""
	}
	a = strings.ToLower(a)
	if len(a) > 253 {
		return ""
	}
	labels := strings.Split(a, ".")
	if len(labels) < 2 {
		return ""
	}
	for _, l := range labels {
		if !ldhLabel(l) {
			return ""
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return ""
	}
	return a
}

// ldhLabel : label DNS strict (lettres minuscules, chiffres, tirets internes ;
// 63 caractères au plus).
func ldhLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Suggest : région suggérée (suggested, "" : aucune), puis zone, pays, ville
// et adresse de mesure prises au catalogue ; la suggérée passe en tête,
// l'ordre reste sinon.
func Suggest(rs []Region, suggested string, catalog map[string]RegionSetting) []Region {
	for i := range rs {
		rs[i].Suggested = suggested != "" && rs[i].Code == suggested
		e := catalog[rs[i].Code]
		rs[i].Zone, rs[i].Country, rs[i].City, rs[i].ProbeURL = e.Zone, e.Country, e.City, e.ProbeURL
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
