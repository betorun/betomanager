package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Carte des fonctionnalités (lot A2) : tout ce que betorun sait faire,
// générée depuis les routes publiques et leur décision pour le Copilot
// (outil, carte d'action ou écran de la console). Le Copilot la consulte
// avant d'affirmer qu'une fonctionnalité existe ou de citer un écran : il ne
// peut ni en inventer une, ni ignorer une qui existe.

const ToolFindFeatures = "find_features"

// FeatureRoute : une route publique connue du Copilot, autre qu'un outil
// (guide : faite par la personne dans la console).
type FeatureRoute struct {
	Method, Path  string
	DescriptionFR string
	Screen        string
	Note          string
	Sensitive     bool
}

// Screen : écran de la console.
type Screen struct {
	Path, Title, TitleFR string
}

// Feature : une fonctionnalité de la carte.
type Feature struct {
	Area      string `json:"area"`
	Summary   string `json:"summary"`
	SummaryFR string `json:"-"`
	// Tool : outil à appeler (lecture, ou carte que la personne valide).
	Tool   string `json:"tool,omitempty"`
	Action bool   `json:"action,omitempty"`
	// Screen : écran de la console où la personne le fait elle-même.
	Screen    string `json:"screen,omitempty"`
	Sensitive bool   `json:"needs_two_factor,omitempty"`
	Note      string `json:"note,omitempty"`
	text      string
}

var findFeaturesSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What the person wants to do or know, in a few keywords (any language). Empty: the list of areas and console screens."}},"required":["query"],"additionalProperties":false}`)

// featureMap : fonctionnalités et écrans, prêts pour la recherche.
type featureMap struct {
	features []Feature
	screens  []Screen
}

// AddFeatureMap : outil find_features. Les fonctionnalités des outils viennent
// du catalogue ; celles des guides, de leurs routes ; résumés et sections, de
// la spécification publique (anglais) ; descriptions françaises, des routes.
func AddFeatureMap(c *Catalog, rawSpec []byte, routesFR map[string]string, guides []FeatureRoute, screens []Screen) error {
	var s spec
	if err := json.Unmarshal(rawSpec, &s); err != nil {
		return fmt.Errorf("copilot: spécification illisible : %w", err)
	}
	op := func(method, path string) (specOperation, bool) {
		ops, ok := s.Paths[openAPIPath(path)]
		if !ok {
			return specOperation{}, false
		}
		o, ok := ops[strings.ToLower(method)]
		return o, ok
	}
	area := func(o specOperation) string {
		if len(o.Tags) > 0 {
			return o.Tags[0]
		}
		return "Other"
	}
	m := &featureMap{screens: screens}
	for _, name := range c.order {
		t := c.tools[name]
		if t.Method == "" {
			continue // outil composé : décrit à part
		}
		o, ok := op(t.Method, t.Path)
		if !ok {
			return fmt.Errorf("copilot: carte : outil %q absent de la spécification", name)
		}
		m.features = append(m.features, Feature{Area: area(o), Summary: firstParagraph(firstNonEmpty(o.Summary, o.Description)),
			SummaryFR: routesFR[t.Method+" "+t.Path], Tool: name, Action: t.Write, Sensitive: t.Sensitive})
	}
	for _, g := range guides {
		o, ok := op(g.Method, g.Path)
		summary, a := g.DescriptionFR, "Other"
		if ok {
			summary, a = firstParagraph(firstNonEmpty(o.Summary, o.Description)), area(o)
		}
		m.features = append(m.features, Feature{Area: a, Summary: summary, SummaryFR: g.DescriptionFR,
			Screen: g.Screen, Sensitive: g.Sensitive, Note: g.Note})
	}
	m.features = append(m.features, Feature{Area: "Containers", Tool: ToolDiagnoseWorkload,
		Summary:   "Diagnose a container that is down, crashing, slow or unreachable (configuration, live state, events, logs)",
		SummaryFR: "Diagnostiquer un conteneur en panne, qui redémarre, lent ou injoignable"})
	for i := range m.features {
		f := &m.features[i]
		f.text = fold(strings.Join([]string{f.Area, f.Summary, f.SummaryFR, f.Tool, f.Screen, f.Note}, " "))
	}
	sort.SliceStable(m.features, func(i, j int) bool { return m.features[i].Area < m.features[j].Area })
	return c.Add(&Tool{
		Route: Route{Name: ToolFindFeatures},
		Description: "The catalog of EVERYTHING betorun can do, and how: which tool to call, whether it is an action card " +
			"the person validates, or a console screen where the person does it (deletions, sensitive actions, file uploads, " +
			"secrets). Call it BEFORE saying that betorun can or cannot do something, before giving a console path, and when " +
			"you do not know which tool fits. If it returns no match, betorun does not offer it: say so, never invent a " +
			"feature, setting, screen or command.",
		Schema: findFeaturesSchema,
		run: func(_ context.Context, _ *Catalog, _ Invoker, args map[string]any) Result {
			return m.find(scalar(args["query"]))
		},
	})
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// fold : minuscules sans accents.
func fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return strings.ToLower(s)
	}
	return out
}

// stop : mots trop courants pour départager.
var stop = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true, "can": true, "what": true, "this": true, "that": true,
	"des": true, "les": true, "une": true, "pour": true, "avec": true, "comment": true, "est": true, "dans": true,
	"sur": true, "mon": true, "mes": true, "faire": true, "veux": true, "peux": true, "quoi": true, "qui": true,
}

func keywords(q string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(fold(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 3 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

// stem : racine grossière (pluriels, accords), pour « bases » ~ « base ».
func stem(w string) string {
	for _, suf := range []string{"ies", "es", "s", "e"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

const maxFeatures = 15

func (m *featureMap) find(query string) Result {
	words := keywords(query)
	var b strings.Builder
	if len(words) == 0 {
		areas := map[string]int{}
		for _, f := range m.features {
			areas[f.Area]++
		}
		names := make([]string, 0, len(areas))
		for a := range areas {
			names = append(names, a)
		}
		sort.Strings(names)
		b.WriteString("Areas of betorun (number of features):\n")
		for _, a := range names {
			fmt.Fprintf(&b, "- %s (%d)\n", a, areas[a])
		}
		m.writeScreens(&b, nil)
		return Result{Content: b.String()}
	}
	type hit struct {
		f     Feature
		score int
	}
	var hits []hit
	for _, f := range m.features {
		score := 0
		for _, w := range words {
			if strings.Contains(f.text, stem(w)) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{f, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) == 0 {
		fmt.Fprintf(&b, "NO MATCH for %q: betorun has no feature for this in its catalog. Tell the person it is not offered "+
			"(do not invent a feature, setting, screen or workaround); offer create_support_ticket if they need it.\n", query)
		m.writeScreens(&b, words)
		return Result{Content: b.String()}
	}
	best := hits[0].score
	shown := 0
	for _, h := range hits {
		if shown == maxFeatures || h.score < best && shown >= 5 {
			break
		}
		f := h.f
		fmt.Fprintf(&b, "- [%s] %s → ", f.Area, f.Summary)
		switch {
		case f.Tool != "" && f.Action:
			fmt.Fprintf(&b, "action card: %s (you propose it, the person validates)", f.Tool)
		case f.Tool != "":
			fmt.Fprintf(&b, "read tool: %s", f.Tool)
		default:
			fmt.Fprintf(&b, "done by the person in the console: %s", f.Screen)
		}
		if f.Sensitive {
			b.WriteString(" (needs two-factor authentication)")
		}
		if f.Note != "" {
			fmt.Fprintf(&b, " — note: %s", f.Note)
		}
		b.WriteString("\n")
		shown++
	}
	if len(hits) > shown {
		fmt.Fprintf(&b, "(%d other weaker matches: refine the query if none of these fits)\n", len(hits)-shown)
	}
	m.writeScreens(&b, words)
	return Result{Content: b.String()}
}

// writeScreens : écrans de la console (tous, ou ceux qui correspondent).
func (m *featureMap) writeScreens(b *strings.Builder, words []string) {
	var lines []string
	for _, s := range m.screens {
		text := fold(s.Path + " " + s.Title + " " + s.TitleFR)
		match := len(words) == 0
		for _, w := range words {
			if strings.Contains(text, stem(w)) {
				match = true
			}
		}
		if match {
			lines = append(lines, fmt.Sprintf("- %s: %s", s.Path, s.Title))
		}
	}
	if len(lines) > 0 {
		b.WriteString("Console screens (link them as relative Markdown links, only these paths exist):\n")
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
}

// namedToolRe : outils nommés dans une réponse de find_features.
var namedToolRe = regexp.MustCompile(`(?:read tool|action card): ([a-z][a-z0-9_]+)`)

// NamedTools : outils qu'une réponse de find_features met à disposition.
func NamedTools(findResult string) []string {
	var out []string
	for _, m := range namedToolRe.FindAllStringSubmatch(findResult, -1) {
		out = append(out, m[1])
	}
	return out
}
