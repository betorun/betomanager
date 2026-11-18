// Package tools : outils du Copilot.
//
// Un outil n'est pas du code à part : c'est une ROUTE de l'API, marquée
// ForCopilot dans la table de routes, que le Copilot rejoue en interne avec
// le jeton délégué de la personne (sts.MethodDelegated). Droits IAM, refus des
// actions sensibles, quotas et journal d'audit s'appliquent donc exactement
// comme pour la console — sans une ligne de contrôle dupliquée ici.
//
// Le schéma de chaque outil (paramètres, descriptions) vient de la
// spécification OpenAPI publique : une route documentée est un outil décrit.
// Lot 1 : lectures seulement (GET), jeton délégué en portée « read ».
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// Route : une route marquée ForCopilot (déclarée par le routeur).
type Route struct {
	Name   string // nom de l'outil (snake_case)
	Method string
	Path   string // chemin gin complet, ex. /kmanager/v1/workloads/:id/status
	// Fixed : paramètres de requête imposés, invisibles du modèle
	// (ex. follow=false sur un flux de journaux).
	Fixed map[string]string
	// Max : plafond de paramètres entiers (ex. tail ≤ 200 lignes).
	Max map[string]int
	// Hint : précision ajoutée à la description, pour le modèle.
	Hint string
}

// Tool : un outil prêt à être proposé au modèle et exécuté.
type Tool struct {
	Route
	Description string
	// Schema : schéma JSON (objet) des arguments.
	Schema     json.RawMessage
	pathParams []string
	query      map[string]bool
	// run : outil composite (plusieurs routes), nil pour un outil-route.
	run func(ctx context.Context, c *Catalog, inv Invoker, args map[string]any) Result
}

// Catalog : outils disponibles, par nom.
type Catalog struct {
	tools map[string]*Tool
	order []string
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,48}$`)

// Lookup : outil par nom.
func (c *Catalog) Lookup(name string) (*Tool, bool) {
	t, ok := c.tools[name]
	return t, ok
}

// Definitions : outils au format llm, dans un ordre stable (cache de prompt).
func (c *Catalog) Definitions() []llm.Tool {
	out := make([]llm.Tool, 0, len(c.order))
	for _, n := range c.order {
		t := c.tools[n]
		out = append(out, llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	return out
}

// Add : ajoute un outil composite (ex. diagnose_workload).
func (c *Catalog) Add(t *Tool) error {
	if !nameRe.MatchString(t.Name) {
		return fmt.Errorf("copilot: nom d'outil invalide %q", t.Name)
	}
	if _, dup := c.tools[t.Name]; dup {
		return fmt.Errorf("copilot: outil en double %q", t.Name)
	}
	c.tools[t.Name] = t
	c.order = append(c.order, t.Name)
	sort.Strings(c.order)
	return nil
}

// ── Construction depuis la spécification OpenAPI ───────────────────────────

type specParam struct {
	Name        string          `json:"name"`
	In          string          `json:"in"`
	Required    bool            `json:"required"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type specOperation struct {
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Parameters  []specParam `json:"parameters"`
}

type spec struct {
	Paths map[string]map[string]specOperation `json:"paths"`
}

var ginParamRe = regexp.MustCompile(`:([A-Za-z0-9_]+)`)

// openAPIPath : /workloads/:id → /workloads/{id}.
func openAPIPath(ginPath string) string {
	return ginParamRe.ReplaceAllString(ginPath, "{$1}")
}

// Build : un outil par route, décrit par l'opération correspondante de la
// spécification (publique, de préférence en anglais : la langue de la
// réponse est fixée par le prompt, pas par les descriptions).
func Build(routes []Route, rawSpec []byte) (*Catalog, error) {
	var s spec
	if err := json.Unmarshal(rawSpec, &s); err != nil {
		return nil, fmt.Errorf("copilot: spécification illisible : %w", err)
	}
	c := &Catalog{tools: map[string]*Tool{}}
	for _, r := range routes {
		if r.Method != http.MethodGet {
			// Lot 1 : lecture seule. Les écritures passeront par une carte
			// validée par la personne (lot 2), jamais par un outil direct.
			return nil, fmt.Errorf("copilot: %s %s : seules les lectures (GET) sont des outils", r.Method, r.Path)
		}
		ops, ok := s.Paths[openAPIPath(r.Path)]
		if !ok {
			return nil, fmt.Errorf("copilot: outil %q : %s absent de la spécification publique", r.Name, r.Path)
		}
		op, ok := ops[strings.ToLower(r.Method)]
		if !ok {
			return nil, fmt.Errorf("copilot: outil %q : %s %s absent de la spécification", r.Name, r.Method, r.Path)
		}
		t, err := fromOperation(r, op)
		if err != nil {
			return nil, err
		}
		if err := c.Add(t); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func fromOperation(r Route, op specOperation) (*Tool, error) {
	props := map[string]json.RawMessage{}
	required := []string{} // jamais null : certains fournisseurs le refusent
	t := &Tool{Route: r, query: map[string]bool{}}
	for _, p := range op.Parameters {
		if _, fixed := r.Fixed[p.Name]; fixed {
			continue
		}
		schema := map[string]any{}
		if len(p.Schema) > 0 {
			if err := json.Unmarshal(p.Schema, &schema); err != nil {
				return nil, fmt.Errorf("copilot: outil %q, paramètre %q : schéma illisible", r.Name, p.Name)
			}
		}
		if schema["type"] == nil {
			schema["type"] = "string"
		}
		if p.Description != "" {
			schema["description"] = p.Description
		} else if p.In == "path" {
			schema["description"] = "Identifier (UUID), as returned by a list or get tool"
		}
		if max, ok := r.Max[p.Name]; ok {
			schema["maximum"] = max
		}
		raw, _ := json.Marshal(schema)
		props[p.Name] = raw
		switch p.In {
		case "path":
			t.pathParams = append(t.pathParams, p.Name)
			required = append(required, p.Name)
		case "query":
			t.query[p.Name] = true
			if p.Required {
				required = append(required, p.Name)
			}
		default:
			continue // en-têtes : jamais exposés au modèle
		}
	}
	for _, name := range pathParamsOf(r.Path) {
		if _, ok := props[name]; !ok {
			return nil, fmt.Errorf("copilot: outil %q : paramètre de chemin %q non documenté", r.Name, name)
		}
	}
	sort.Strings(required)
	schema, err := json.Marshal(map[string]any{
		"type": "object", "properties": props, "required": required, "additionalProperties": false,
	})
	if err != nil {
		return nil, err
	}
	t.Schema = schema
	// La description complète contient déjà le résumé (le résumé en est le
	// début, avant la première parenthèse) : on ne prend que l'une.
	desc := firstParagraph(op.Description)
	if desc == "" {
		desc = strings.TrimSpace(op.Summary)
	}
	if desc != "" && !strings.HasSuffix(desc, ".") {
		desc += "."
	}
	if r.Hint != "" {
		desc += " " + r.Hint
	}
	if desc == "" {
		return nil, errors.New("copilot: outil " + r.Name + " sans description")
	}
	t.Description = desc
	return t, nil
}

func pathParamsOf(ginPath string) []string {
	var out []string
	for _, m := range ginParamRe.FindAllStringSubmatch(ginPath, -1) {
		out = append(out, m[1])
	}
	return out
}

// firstParagraph : la description sans la mention d'accès ajoutée par la doc
// (« **Access**: … »), inutile au modèle.
func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
