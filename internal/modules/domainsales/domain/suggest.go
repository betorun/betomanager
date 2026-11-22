package domain

import "strings"

// Suggestions : à partir d'un nom recherché, d'autres noms à proposer (comme
// les grands registraires) : le même nom sur d'autres extensions, puis des
// variantes du nom. Rien n'est inventé côté fournisseur : chaque candidat est
// vérifié et tarifé comme une recherche ordinaire.

// DefaultSuggestTLDs : extensions proposées, dans l'ordre d'affichage
// (configurables : domains.suggest_tlds).
var DefaultSuggestTLDs = []string{"com", "net", "org", "fr", "io", "africa", "tech", "app", "online", "shop", "store", "info"}

// Variantes du nom (préfixes, suffixes), sur l'extension recherchée.
var (
	suggestPrefixes = []string{"get", "mon"}
	suggestSuffixes = []string{"app", "hq", "online"}
)

// MaxSuggestions : candidats vérifiés au plus par recherche.
const MaxSuggestions = 16

// Kinds de suggestion.
const (
	SuggestExtension = "extension"
	SuggestVariant   = "variant"
)

// Candidate : un nom à vérifier.
type Candidate struct {
	Domain string
	Kind   string
}

// SplitQuery : « Bomba Shop » ou « bombashop » → (bombashop, com) ;
// « bombashop.cg » → (bombashop, cg). Le libellé seul garde lettres,
// chiffres et tirets. ok faux : rien d'exploitable.
func SplitQuery(raw string, defaultTLD string) (label, tld string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i] // adresse collée : le chemin ne compte pas
	}
	s = strings.TrimPrefix(s, "www.")
	s = strings.TrimSuffix(s, ".")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		label, tld = s[:i], s[i+1:]
	} else {
		label, tld = s, defaultTLD
	}
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_':
			// « bomba shop » → bombashop
		}
	}
	label = strings.Trim(b.String(), "-")
	if label == "" || NormalizeDomain(label+"."+tld) == "" {
		return "", "", false
	}
	return label, tld, true
}

// SuggestCandidates : extensions d'abord (dans l'ordre de tlds), puis
// variantes sur l'extension recherchée ; sans doublon ni le nom exact.
func SuggestCandidates(label, tld string, tlds []string) []Candidate {
	exact := label + "." + tld
	seen := map[string]bool{exact: true}
	var out []Candidate
	add := func(d, kind string) {
		d = NormalizeDomain(d)
		if d == "" || seen[d] || len(out) >= MaxSuggestions {
			return
		}
		seen[d] = true
		out = append(out, Candidate{Domain: d, Kind: kind})
	}
	for _, t := range tlds {
		add(label+"."+strings.TrimPrefix(strings.ToLower(t), "."), SuggestExtension)
	}
	for _, p := range suggestPrefixes {
		add(p+label+"."+tld, SuggestVariant)
	}
	for _, s := range suggestSuffixes {
		add(label+s+"."+tld, SuggestVariant)
	}
	return out
}

// Suggestion : un nom proposé, tarifé comme une recherche.
type Suggestion struct {
	Quote
	Kind string `json:"kind" description:"extension (même nom, autre extension) ou variant (variante du nom)"`
}

// SuggestionsResponse : le nom retenu et ses suggestions vérifiées.
type SuggestionsResponse struct {
	// Query : nom exact retenu (« bombashop.com » pour « bomba shop »).
	Query       string       `json:"query"`
	Suggestions []Suggestion `json:"suggestions"`
}

// SuggestRequest : nom ou mot-clé recherché.
type SuggestRequest struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	Domain         string `query:"domain" description:"Nom ou mot-clé recherché (ex. mon-site, mon-site.com)"`
}
