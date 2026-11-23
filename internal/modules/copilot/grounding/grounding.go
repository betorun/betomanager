// Package grounding : vérification des réponses du Copilot (lot A3). Chaque
// élément vérifiable d'une réponse (identifiant, adresse IP, montant, lien
// vers la console, adresse web, nom d'outil interne) doit venir d'une preuve :
// un résultat d'outil du tour, le message ou la page de la personne, ou un
// échange précédent de la conversation. Sinon c'est une invention.
//
// Les preuves ne sont que des jetons (identifiants, adresses, nombres, hôtes),
// jamais le texte des résultats : la mémoire de la conversation ne devient pas
// un second stockage des journaux ou des configurations.
package grounding

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Sortes d'éléments vérifiés.
const (
	KindID       = "identifier"
	KindIP       = "ip_address"
	KindAmount   = "amount"
	KindLink     = "console_link"
	KindURL      = "url"
	KindToolName = "internal_tool_name"
)

// Violation : un élément de la réponse qu'aucune preuve ne porte.
type Violation struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Evidence : jetons prouvés (identifiants, adresses IP, nombres, hôtes).
type Evidence struct{ tokens map[string]bool }

func NewEvidence() *Evidence { return &Evidence{tokens: map[string]bool{}} }

// MaxTokens : jetons gardés au plus (les plus anciens d'abord écartés par l'appelant).
const MaxTokens = 4000

var (
	uuidRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	ipRe   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// numRe : nombres, séparateurs de milliers (espace, espace fine, point,
	// virgule) et décimales compris.
	numRe  = regexp.MustCompile(`\d[\d\x{00A0}\x{202F}\x{2009} .,']*\d|\d`)
	hostRe = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,24}\b`)
)

// Add : jetons d'un texte (résultat d'outil, message, page).
func (e *Evidence) Add(text string) {
	for _, t := range tokens(text) {
		if len(e.tokens) >= MaxTokens {
			return
		}
		e.tokens[t] = true
	}
}

// AddTokens : jetons déjà extraits (mémoire de la conversation).
func (e *Evidence) AddTokens(ts []string) {
	for _, t := range ts {
		if len(e.tokens) < MaxTokens {
			e.tokens[t] = true
		}
	}
}

// Tokens : jetons, triés (à garder avec la conversation).
func (e *Evidence) Tokens() []string {
	out := make([]string, 0, len(e.tokens))
	for t := range e.tokens {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (e *Evidence) has(t string) bool { return e.tokens[t] }

// tokens : identifiants, adresses IP, nombres normalisés, hôtes.
func tokens(text string) []string {
	var out []string
	for _, m := range uuidRe.FindAllString(text, -1) {
		out = append(out, "id:"+strings.ToLower(m))
	}
	for _, m := range ipRe.FindAllString(text, -1) {
		if a, err := netip.ParseAddr(m); err == nil {
			out = append(out, "ip:"+a.String())
		}
	}
	for _, m := range numRe.FindAllString(text, -1) {
		for _, n := range numberForms(m) {
			out = append(out, "n:"+n)
		}
	}
	for _, m := range hostRe.FindAllString(text, -1) {
		out = append(out, "h:"+strings.ToLower(m))
	}
	return out
}

// numberForms : écritures possibles d'un nombre (« 1,500.50 », « 1 500,5 »,
// « 1500.5 » → 1500.5) ; un nombre ambigu donne ses deux lectures.
func numberForms(raw string) []string {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', ' ', ' ', ' ', '\'':
			return -1
		}
		return r
	}, raw)
	s = strings.Trim(s, ".,")
	if s == "" {
		return nil
	}
	var forms []string
	lastDot, lastComma := strings.LastIndexByte(s, '.'), strings.LastIndexByte(s, ',')
	// Le dernier séparateur suivi de 1 à 2 chiffres est un séparateur décimal.
	decimal := func(sep int) bool { return sep >= 0 && len(s)-sep-1 >= 1 && len(s)-sep-1 <= 2 }
	switch {
	case lastDot > lastComma && decimal(lastDot):
		forms = append(forms, clean(strings.ReplaceAll(s[:lastDot], ",", "")+"."+s[lastDot+1:]))
	case lastComma > lastDot && decimal(lastComma):
		forms = append(forms, clean(strings.ReplaceAll(s[:lastComma], ".", "")+"."+s[lastComma+1:]))
	}
	forms = append(forms, clean(strings.NewReplacer(".", "", ",", "").Replace(s)))
	return forms
}

// clean : sans zéros inutiles (« 05 » → « 5 », « 2.50 » → « 2.5 »).
func clean(n string) string {
	intPart, frac, hasFrac := strings.Cut(n, ".")
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	if hasFrac {
		frac = strings.TrimRight(frac, "0")
		if frac != "" {
			return intPart + "." + frac
		}
	}
	return intPart
}

// Rules : ce que la réponse peut citer sans preuve.
type Rules struct {
	// Screens : écrans fixes de la console (/billing, /network…).
	Screens map[string]bool
	// ResourcePages : préfixes des pages d'une ressource (/workloads/…) :
	// l'identifiant qui suit doit être prouvé.
	ResourcePages []string
	// ToolNames : noms internes, jamais montrés à la personne.
	ToolNames map[string]bool
	// Hosts : hôtes citables sans preuve (documentation de betorun…).
	Hosts map[string]bool
}

var (
	// amountRe : nombre accolé à une monnaie (avant ou après).
	amountRe = regexp.MustCompile(`(?i)(?:(?:XAF|FCFA|CFA|EUR|USD|€|\$)\s?(\d[\d\x{00A0}\x{202F}\x{2009} .,']*\d|\d))|(?:(\d[\d\x{00A0}\x{202F}\x{2009} .,']*\d|\d)\s?(?:XAF|FCFA|F\s?CFA|CFA|EUR|USD|€|\$|euros?|dollars?|francs?))`)
	// linkRe : lien Markdown relatif [texte](/chemin).
	linkRe = regexp.MustCompile(`\]\((/[^)\s]*)\)`)
	urlRe  = regexp.MustCompile(`(?i)\bhttps?://([a-z0-9.-]+)`)
	// codeRe : texte entre accents graves (noms d'outils montrés).
	codeRe = regexp.MustCompile("`([a-z][a-z0-9_]+)`|\\b([a-z]+_[a-z0-9_]+)\\b")
)

// Check : éléments de la réponse sans preuve.
func Check(answer string, ev *Evidence, rules Rules) []Violation {
	var out []Violation
	seen := map[string]bool{}
	add := func(kind, value string) {
		if k := kind + "|" + value; !seen[k] {
			seen[k] = true
			out = append(out, Violation{Kind: kind, Value: value})
		}
	}
	for _, m := range uuidRe.FindAllString(answer, -1) {
		if !ev.has("id:" + strings.ToLower(m)) {
			add(KindID, m)
		}
	}
	for _, m := range ipRe.FindAllString(answer, -1) {
		a, err := netip.ParseAddr(m)
		if err != nil || a.IsUnspecified() || a.IsLoopback() {
			continue // 0.0.0.0 / 127.0.0.1 : conseil général (écouter sur toutes les interfaces)
		}
		if !ev.has("ip:" + a.String()) {
			add(KindIP, m)
		}
	}
	for _, m := range amountRe.FindAllStringSubmatch(answer, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		proven := false
		for _, n := range numberForms(raw) {
			if ev.has("n:" + n) {
				proven = true
			}
		}
		if !proven {
			add(KindAmount, strings.TrimSpace(m[0]))
		}
	}
	for _, m := range linkRe.FindAllStringSubmatch(answer, -1) {
		if !linkProven(m[1], ev, rules) {
			add(KindLink, m[1])
		}
	}
	for _, m := range urlRe.FindAllStringSubmatch(answer, -1) {
		host := strings.ToLower(strings.TrimRight(m[1], "."))
		if !rules.Hosts[host] && !ev.has("h:"+host) {
			add(KindURL, host)
		}
	}
	for _, m := range codeRe.FindAllStringSubmatch(answer, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if rules.ToolNames[name] {
			add(KindToolName, name)
		}
	}
	return out
}

// linkProven : écran fixe, ou page d'une ressource dont l'identifiant est prouvé.
func linkProven(raw string, ev *Evidence, rules Rules) bool {
	path := raw
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	if path == "" || rules.Screens[path] {
		return true
	}
	for _, prefix := range rules.ResourcePages {
		if rest, ok := strings.CutPrefix(path, prefix); ok && rest != "" && !strings.Contains(rest, "/") {
			return uuidRe.MatchString(rest) && ev.has("id:"+strings.ToLower(rest))
		}
	}
	return false
}

// Sanitize : la réponse sans les éléments non prouvés (dernier recours,
// quand le modèle n'a pas su se corriger). Un lien devient son texte, une
// adresse ou un montant inventé est remplacé par une mention explicite.
func Sanitize(answer string, violations []Violation, unverified string) string {
	out := answer
	for _, v := range violations {
		switch v.Kind {
		case KindLink:
			re := regexp.MustCompile(`\[([^\]]*)\]\(` + regexp.QuoteMeta(v.Value) + `\)`)
			out = re.ReplaceAllString(out, "$1")
		case KindToolName:
			out = strings.ReplaceAll(out, "`"+v.Value+"`", v.Value)
		default:
			out = strings.ReplaceAll(out, v.Value, unverified)
		}
	}
	return out
}

// Feedback : consigne de correction pour le modèle.
func Feedback(violations []Violation) string {
	var b strings.Builder
	b.WriteString("VERIFICATION FAILED. Your answer states things that no tool result, no message of the person and no " +
		"earlier exchange supports. Rewrite your whole answer: remove each item below, or call the tool that gives it and " +
		"use its exact value. Never show internal tool names to the person. Do not mention this verification.\n")
	for _, v := range violations {
		b.WriteString("- " + v.Kind + ": " + v.Value + "\n")
	}
	return b.String()
}
