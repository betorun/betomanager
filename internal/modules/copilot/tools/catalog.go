// Package tools : outils du Copilot.
//
// Un outil n'est pas du code à part : c'est une ROUTE de l'API, marquée
// ForCopilot dans la table de routes, que le Copilot rejoue en interne avec
// le jeton délégué de la personne (sts.MethodDelegated). Droits IAM, refus des
// actions sensibles, quotas et journal d'audit s'appliquent donc exactement
// comme pour la console — sans une ligne de contrôle dupliquée ici.
//
// Le schéma de chaque outil (paramètres, corps, descriptions) vient de la
// spécification OpenAPI publique : une route documentée est un outil décrit.
//
// Lectures : exécutées aussitôt (jeton en portée « read »).
// Écritures (Write) : JAMAIS exécutées par le modèle. Prepare en fait une
// requête figée, présentée à la personne sur une carte ; Execute ne la joue
// qu'après sa validation, avec un jeton en portée « write ».
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

// Risk : gravité d'une écriture, pour la carte de validation.
type Risk string

const (
	RiskLow    Risk = "low"    // sans interruption (redémarrage progressif, domaine ajouté)
	RiskMedium Risk = "medium" // effet visible (arrêt, retour arrière, nouveau déploiement)
	// RiskHigh : carte sensible (Strong) : suppression, données remplacées,
	// argent, accès ; la personne confirme avec sa double authentification.
	RiskHigh Risk = "high"
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

	// Écritures.
	Write bool
	Risk  Risk
	// Omit : champs du corps retirés du schéma (ex. « secrets » : une valeur
	// secrète ne transite jamais par le modèle).
	Omit []string
	// Only : liste blanche des champs du corps exposés (prioritaire sur Omit).
	// Pour les créations : le modèle ne règle que l'essentiel, le reste garde
	// les valeurs par défaut de l'API.
	Only []string
	// Undo : outil inverse, rejoué avec les mêmes paramètres de chemin
	// (ex. stop_workload ↔ start_workload).
	Undo string
	// Sensitive : la permission de la route exige la double authentification.
	// Une délégation ne peut pas la satisfaire : seulement en carte Strong.
	Sensitive bool
	// Strong : carte sensible. À la validation, l'action est jouée avec la
	// session de la personne elle-même (jamais un jeton délégué) : l'API exige
	// alors sa confirmation récente (double authentification) comme dans la
	// console. Obligatoire pour une action sensible ou une suppression.
	Strong bool
	// ConfirmName : la personne retape le nom de la ressource avant de
	// valider (suppressions, données remplacées).
	ConfirmName bool
	// TargetTool : outil de lecture qui donne le nom de la ressource visée
	// (mêmes paramètres de chemin) ; vide : règle par défaut.
	TargetTool string
}

// Tool : un outil prêt à être proposé au modèle et exécuté.
type Tool struct {
	Route
	Description string
	// Schema : schéma JSON (objet) des arguments.
	Schema     json.RawMessage
	pathParams []string
	query      map[string]bool
	body       map[string]bool
	// run : outil composite (plusieurs routes), nil pour un outil-route.
	run func(ctx context.Context, c *Catalog, inv Invoker, args map[string]any) Result
}

// PathParams : paramètres de chemin (identifiants de la ressource visée).
func (t *Tool) PathParams() map[string]bool {
	out := make(map[string]bool, len(t.pathParams))
	for _, p := range t.pathParams {
		out[p] = true
	}
	return out
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

// CoreTools : outils présentés au modèle dès le début d'un tour. Les autres
// (plus d'une centaine) le sont quand find_features les nomme : un catalogue
// entier à chaque appel coûterait ~15 000 jetons, dépasserait la limite de
// certains fournisseurs (128 outils) et égarerait le modèle.
var CoreTools = []string{
	ToolFindFeatures, ToolDiagnoseWorkload,
	"list_projects", "get_project_status", "get_project_network", "get_project_insights",
	"list_workloads", "get_workload", "get_workload_status", "get_workload_logs", "get_workload_events",
	"list_builds", "get_build", "get_build_logs", "build_from_git", "create_workload", "restart_workload",
	"list_databases", "list_addons", "list_sites",
	// Parcours « mettre en ligne » (lot B) : lire GitHub, proposer UN plan.
	ToolListGitHubAccounts, ToolListGitHubRepositories, ToolListGitHubBranches, ToolProposeLaunchPlan,
	"get_price_catalog", "get_platform_status", "create_support_ticket",
}

// Names : noms de tous les outils (vérification des réponses : jamais montrés).
func (c *Catalog) Names() []string { return append([]string(nil), c.order...) }

// Core : outils du noyau présents dans ce catalogue.
func (c *Catalog) Core() []string {
	out := make([]string, 0, len(CoreTools))
	for _, n := range CoreTools {
		if _, ok := c.tools[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// DefinitionsOf : définitions de ces outils (inconnus ignorés), ordre du catalogue.
func (c *Catalog) DefinitionsOf(names []string) []llm.Tool {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := make([]llm.Tool, 0, len(names))
	for _, n := range c.order {
		if want[n] {
			t := c.tools[n]
			out = append(out, llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Schema})
		}
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

type specMedia struct {
	Schema json.RawMessage `json:"schema"`
}

type specOperation struct {
	Tags        []string    `json:"tags"`
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Parameters  []specParam `json:"parameters"`
	RequestBody *struct {
		Content map[string]specMedia `json:"content"`
	} `json:"requestBody"`
}

type spec struct {
	Paths      map[string]map[string]specOperation `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
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
		if err := checkRoute(r); err != nil {
			return nil, err
		}
		ops, ok := s.Paths[openAPIPath(r.Path)]
		if !ok {
			return nil, fmt.Errorf("copilot: outil %q : %s absent de la spécification publique", r.Name, r.Path)
		}
		op, ok := ops[strings.ToLower(r.Method)]
		if !ok {
			return nil, fmt.Errorf("copilot: outil %q : %s %s absent de la spécification", r.Name, r.Method, r.Path)
		}
		t, err := fromOperation(r, op, s.Components.Schemas)
		if err != nil {
			return nil, err
		}
		if err := c.Add(t); err != nil {
			return nil, err
		}
	}
	for _, t := range c.tools {
		if t.Undo == "" {
			continue
		}
		inv, ok := c.tools[t.Undo]
		if !ok || !inv.Write || strings.Join(inv.pathParams, ",") != strings.Join(t.pathParams, ",") {
			return nil, fmt.Errorf("copilot: outil %q : inverse %q absent ou incompatible", t.Name, t.Undo)
		}
	}
	// Outil qui lit le nom de la cible : une lecture dont les paramètres de
	// chemin sont tous fournis par la carte.
	for _, t := range c.tools {
		if t.TargetTool == "" {
			continue
		}
		rd, ok := c.tools[t.TargetTool]
		if !ok || rd.Write {
			return nil, fmt.Errorf("copilot: outil %q : lecture de la cible %q absente", t.Name, t.TargetTool)
		}
		mine := t.PathParams()
		for _, p := range rd.pathParams {
			if !mine[p] {
				return nil, fmt.Errorf("copilot: outil %q : %q exige le paramètre %q", t.Name, t.TargetTool, p)
			}
		}
	}
	return c, nil
}

// checkRoute : une lecture est un GET ; une écriture n'est jamais un GET. Une
// suppression ou une action sensible n'est qu'une carte Strong (jouée avec la
// session de la personne, double authentification comprise), de risque
// « high » ; une suppression fait retaper le nom.
func checkRoute(r Route) error {
	switch {
	case !r.Write && r.Method != http.MethodGet:
		return fmt.Errorf("copilot: %s %s : seules les lectures (GET) sont des outils", r.Method, r.Path)
	case !r.Write && (r.Strong || r.ConfirmName):
		return fmt.Errorf("copilot: outil %q : une lecture n'est pas une carte sensible", r.Name)
	case r.Write && r.Method == http.MethodGet:
		return fmt.Errorf("copilot: outil %q : écriture %s refusée", r.Name, r.Method)
	case r.Write && (r.Sensitive || r.Method == http.MethodDelete) && !r.Strong:
		return fmt.Errorf("copilot: outil %q : action sensible ou suppression : carte Strong seulement", r.Name)
	case r.ConfirmName && r.TargetTool == "":
		return fmt.Errorf("copilot: outil %q : nom à retaper sans outil qui le lit (TargetTool)", r.Name)
	case r.Strong && r.Risk != RiskHigh:
		return fmt.Errorf("copilot: outil %q : carte sensible : risque high", r.Name)
	case r.Write && !r.Strong && r.Risk != RiskLow && r.Risk != RiskMedium:
		return fmt.Errorf("copilot: outil %q : risque non déclaré", r.Name)
	}
	return nil
}

func fromOperation(r Route, op specOperation, components map[string]json.RawMessage) (*Tool, error) {
	props := map[string]json.RawMessage{}
	required := []string{} // jamais null : certains fournisseurs le refusent
	t := &Tool{Route: r, query: map[string]bool{}, body: map[string]bool{}}
	for _, p := range op.Parameters {
		if _, fixed := r.Fixed[p.Name]; fixed {
			continue
		}
		if p.In != "path" && p.In != "query" {
			continue // en-têtes : jamais exposés au modèle
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
		if p.In == "path" {
			t.pathParams = append(t.pathParams, p.Name)
			required = append(required, p.Name)
		} else {
			t.query[p.Name] = true
			if p.Required {
				required = append(required, p.Name)
			}
		}
	}
	for _, name := range pathParamsOf(r.Path) {
		if _, ok := props[name]; !ok {
			return nil, fmt.Errorf("copilot: outil %q : paramètre de chemin %q non documenté", r.Name, name)
		}
	}

	if r.Write && op.RequestBody != nil {
		if err := addBody(t, op, components, props, &required); err != nil {
			return nil, err
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
	if r.Write {
		desc += " ACTION: calling this tool does NOT run it; it shows the person a card to validate."
	}
	if r.Strong {
		desc += " SENSITIVE: when validating, the person confirms with their two-factor authentication in the console" +
			" (never ask for a code in the chat)."
	}
	if r.ConfirmName {
		desc += " IRREVERSIBLE: the person retypes the resource name on the card; warn them clearly of what is lost."
	}
	if r.Hint != "" {
		desc += " " + r.Hint
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return nil, errors.New("copilot: outil " + r.Name + " sans description")
	}
	t.Description = desc
	return t, nil
}

// addBody : les champs du corps JSON deviennent des arguments de premier
// niveau (sauf Omit), schéma résolu depuis les composants de la spécification.
func addBody(t *Tool, op specOperation, components map[string]json.RawMessage, props map[string]json.RawMessage, required *[]string) error {
	media, ok := op.RequestBody.Content["application/json"]
	if !ok {
		return fmt.Errorf("copilot: outil %q : corps non JSON", t.Name)
	}
	resolved, err := resolveRefs(media.Schema, components)
	if err != nil {
		return fmt.Errorf("copilot: outil %q : %w", t.Name, err)
	}
	var body struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(resolved, &body); err != nil {
		return fmt.Errorf("copilot: outil %q : corps illisible", t.Name)
	}
	omit := map[string]bool{}
	for _, o := range t.Omit {
		omit[o] = true
	}
	if len(t.Only) > 0 {
		only := map[string]bool{}
		for _, o := range t.Only {
			only[o] = true
			if _, ok := body.Properties[o]; !ok {
				return fmt.Errorf("copilot: outil %q : champ %q absent du corps", t.Name, o)
			}
		}
		for name := range body.Properties {
			if !only[name] {
				omit[name] = true
			}
		}
	}
	for name, s := range body.Properties {
		if omit[name] {
			continue
		}
		if _, clash := props[name]; clash {
			return fmt.Errorf("copilot: outil %q : champ %q à la fois paramètre et corps", t.Name, name)
		}
		props[name] = s
		t.body[name] = true
	}
	for _, name := range body.Required {
		if !omit[name] {
			*required = append(*required, name)
		}
	}
	return nil
}

// resolveRefs : remplace les « $ref » vers #/components/schemas par leur
// contenu (le modèle reçoit un schéma autonome). Profondeur bornée.
func resolveRefs(raw json.RawMessage, components map[string]json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var walk func(x any, depth int) (any, error)
	walk = func(x any, depth int) (any, error) {
		if depth > 12 {
			return nil, errors.New("schéma trop imbriqué")
		}
		switch t := x.(type) {
		case map[string]any:
			if ref, ok := t["$ref"].(string); ok {
				name, found := strings.CutPrefix(ref, "#/components/schemas/")
				target, ok := components[name]
				if !found || !ok {
					return nil, fmt.Errorf("référence inconnue %q", ref)
				}
				var inner any
				if err := json.Unmarshal(target, &inner); err != nil {
					return nil, err
				}
				return walk(inner, depth+1)
			}
			out := make(map[string]any, len(t))
			for k, val := range t {
				r, err := walk(val, depth+1)
				if err != nil {
					return nil, err
				}
				out[k] = r
			}
			return out, nil
		case []any:
			out := make([]any, len(t))
			for i, val := range t {
				r, err := walk(val, depth+1)
				if err != nil {
					return nil, err
				}
				out[i] = r
			}
			return out, nil
		}
		return x, nil
	}
	res, err := walk(v, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
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
