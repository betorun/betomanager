package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Noms des outils-routes dont dépend le diagnostic.
const (
	ToolGetWorkload       = "get_workload"
	ToolGetWorkloadStatus = "get_workload_status"
	ToolGetWorkloadEvents = "get_workload_events"
	ToolGetWorkloadLogs   = "get_workload_logs"
	ToolDiagnoseWorkload  = "diagnose_workload"
)

var diagnoseSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Container (workload) identifier"}},"required":["id"],"additionalProperties":false}`)

// AddDiagnose : outil composite « pourquoi ce conteneur ne marche pas ? ».
// Un seul appel du modèle rassemble la fiche, l'état en direct, les
// événements récents et la fin des journaux (instance actuelle, puis
// précédente s'il a redémarré). Chaque morceau passe par sa route, avec les
// droits de la personne : un morceau refusé est signalé, pas inventé.
func AddDiagnose(c *Catalog) error {
	for _, dep := range []string{ToolGetWorkload, ToolGetWorkloadStatus, ToolGetWorkloadEvents, ToolGetWorkloadLogs} {
		if _, ok := c.tools[dep]; !ok {
			return fmt.Errorf("copilot: %s exige l'outil %s", ToolDiagnoseWorkload, dep)
		}
	}
	return c.Add(&Tool{
		Route: Route{Name: ToolDiagnoseWorkload},
		Description: "Diagnose a container in one call: its configuration, live state, recent events and " +
			"the end of its logs (current instance, and the previous one if it restarted). Use it first " +
			"when the user asks why a container is down, crashing, slow or not reachable.",
		Schema: diagnoseSchema,
		run:    diagnose,
	})
}

func diagnose(ctx context.Context, c *Catalog, inv Invoker, args map[string]any) Result {
	id := scalar(args["id"])
	if id == "" {
		return errorResult(0, `argument "id" is required`)
	}
	part := func(tool string, extra map[string]any) Result {
		in := map[string]any{"id": id}
		for k, v := range extra {
			in[k] = v
		}
		return c.callRoute(ctx, inv, c.tools[tool], in)
	}

	workload := part(ToolGetWorkload, nil)
	if workload.IsError {
		return workload // introuvable ou refusé : inutile d'aller plus loin
	}
	status := part(ToolGetWorkloadStatus, nil)
	events := part(ToolGetWorkloadEvents, nil)
	logs := part(ToolGetWorkloadLogs, map[string]any{"tail": 120})
	sections := []struct {
		name string
		r    Result
	}{{"workload", workload}, {"status", status}, {"events", events}, {"logs", logs}}
	if restarted(status.Content) {
		sections = append(sections, struct {
			name string
			r    Result
		}{"previous_logs", part(ToolGetWorkloadLogs, map[string]any{"tail": 60, "previous": true})})
	}

	var b strings.Builder
	truncated := false
	for _, s := range sections {
		fmt.Fprintf(&b, "## %s", s.name)
		if s.r.IsError {
			b.WriteString(" (unavailable)")
		}
		b.WriteString("\n")
		b.WriteString(sectionClip(s.r.Content, s.name == "logs" || s.name == "previous_logs"))
		b.WriteString("\n\n")
		truncated = truncated || s.r.Truncated
	}
	return Result{Status: 200, Content: strings.TrimSpace(b.String()), Truncated: truncated}
}

// sectionClip : chaque section garde sa part du budget (4 Kio), pour que des
// journaux bavards n'effacent pas l'état.
func sectionClip(s string, keepTail bool) string {
	const share = 4 << 10
	if len(s) <= share {
		return s
	}
	if keepTail {
		return "[…]\n" + s[len(s)-share:]
	}
	return s[:share] + "\n[…]"
}

// restarted : l'état mentionne des redémarrages (le format exact de l'état
// peut évoluer : on cherche un compteur non nul, sans s'y fier pour le reste).
func restarted(statusJSON string) bool {
	var v any
	if json.Unmarshal([]byte(statusJSON), &v) != nil {
		return false
	}
	found := false
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if strings.Contains(strings.ToLower(k), "restart") {
					if n, ok := val.(float64); ok && n > 0 {
						found = true
					}
				}
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	return found
}
