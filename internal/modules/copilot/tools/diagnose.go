package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thekrauss/kubemanager/internal/modules/copilot/diagnosis"
)

// Noms des outils-routes dont dépend le diagnostic.
const (
	ToolGetWorkload       = "get_workload"
	ToolGetWorkloadStatus = "get_workload_status"
	ToolGetWorkloadEvents = "get_workload_events"
	ToolGetWorkloadLogs   = "get_workload_logs"
	ToolDiagnoseWorkload  = "diagnose_workload"
)

// Lectures du voisinage (facultatives : absentes du catalogue ou refusées,
// le diagnostic le dit « inconnu », sans rien deviner).
const (
	toolListAddons            = "list_addons"
	toolListDatabases         = "list_databases"
	toolListWorkloads         = "list_workloads"
	toolListWorkloadVariables = "list_workload_variables"
	toolGetPriceCatalog       = "get_price_catalog"
)

var diagnoseSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Container (workload) identifier"}},"required":["id"],"additionalProperties":false}`)

// AddDiagnose : outil composite « pourquoi ce conteneur ne marche pas ? ».
// Un seul appel du modèle rassemble la fiche, l'état en direct, les
// événements récents et la fin des journaux (instance actuelle, puis
// précédente s'il a redémarré), et le voisinage du projet (services, bases,
// autres conteneurs, tailles). Chaque morceau passe par sa route, avec les
// droits de la personne : un morceau refusé est signalé, pas inventé. Les
// règles fixes du paquet diagnosis en tirent des constats, rendus EN TÊTE.
func AddDiagnose(c *Catalog) error {
	for _, dep := range []string{ToolGetWorkload, ToolGetWorkloadStatus, ToolGetWorkloadEvents, ToolGetWorkloadLogs} {
		if _, ok := c.tools[dep]; !ok {
			return fmt.Errorf("copilot: %s exige l'outil %s", ToolDiagnoseWorkload, dep)
		}
	}
	return c.Add(&Tool{
		Route: Route{Name: ToolDiagnoseWorkload},
		Description: "Diagnose a container in one call. Starts with a \"findings\" section: causes detected by fixed rules, " +
			"each with its evidence (exact log or event lines, or state fields), params, and sometimes a fix (a tool and its " +
			"arguments, to propose as a card). Then the raw material: configuration, live state, recent events and the end of " +
			"its logs (current instance, and the previous one if it restarted). Use it first when the user asks why a " +
			"container is down, crashing, slow or not reachable.",
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
	var previous *Result
	if restarted(status.Content) {
		p := part(ToolGetWorkloadLogs, map[string]any{"tail": 60, "previous": true})
		previous = &p
		sections = append(sections, struct {
			name string
			r    Result
		}{"previous_logs", p})
	}

	in := diagnosisInput(workload, status, events, logs, previous)
	c.neighborhood(ctx, inv, &in)
	findings := c.checkFixes(diagnosis.Analyze(in))
	raw, _ := json.Marshal(findings)

	var b strings.Builder
	b.WriteString("## findings\n")
	b.Write(raw)
	b.WriteString("\n\n")
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

// checkFixes : une correction n'est gardée que si l'outil visé l'accepte tel
// quel (Catalog.Prepare : outil d'écriture du catalogue, arguments connus).
// Sinon le constat reste, sans correction.
func (c *Catalog) checkFixes(fs []diagnosis.Finding) []diagnosis.Finding {
	for i := range fs {
		f := fs[i].Fix
		if f == nil {
			continue
		}
		t, ok := c.tools[f.Tool]
		raw, err := json.Marshal(f.Args)
		if !ok || !t.Write || err != nil {
			fs[i].Fix = nil
			continue
		}
		if _, err := c.Prepare(f.Tool, raw); err != nil {
			fs[i].Fix = nil
		}
	}
	if fs == nil {
		fs = []diagnosis.Finding{}
	}
	return fs
}

// ── Lecture des réponses des routes (champs utiles seulement) ──────────────

// workloadJSON : fiche d'un conteneur (domain.Workload, champs sans étiquette JSON).
type workloadJSON struct {
	ID, ProjectID, Name, Kind, Image string
	TargetPort                       int
	MemoryLimit, CPULimit            string
	LivenessPath                     string
	ManagedBy                        string
}

type statusJSON struct {
	Status string `json:"status"`
	Kind   string `json:"kind"`
	Pods   []struct {
		RestartCount    int `json:"restart_count"`
		LastTermination *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exit_code"`
		} `json:"last_termination"`
	} `json:"pods"`
	PullFailure *struct {
		Cause    string `json:"cause"`
		Registry string `json:"registry"`
	} `json:"pull_failure"`
}

type attachmentJSON struct {
	WorkloadID string `json:"workload_id"`
	Variable   string `json:"variable"`
}

type serviceJSON struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Template    string           `json:"template"`
	Status      string           `json:"status"`
	Host        string           `json:"host"`
	Port        int              `json:"port"`
	Attachments []attachmentJSON `json:"attachments"`
}

func attachments(in []attachmentJSON) []diagnosis.Attachment {
	out := make([]diagnosis.Attachment, 0, len(in))
	for _, a := range in {
		out = append(out, diagnosis.Attachment{WorkloadID: a.WorkloadID, Variable: a.Variable})
	}
	return out
}

// diagnosisInput : l'entrée du diagnostic, depuis les réponses lues (une
// réponse en erreur ou illisible ne donne rien).
func diagnosisInput(workload, status, events, logs Result, previous *Result) diagnosis.Input {
	var in diagnosis.Input
	var w workloadJSON
	if json.Unmarshal([]byte(workload.Content), &w) == nil {
		in.Workload = diagnosis.Workload{ID: w.ID, ProjectID: w.ProjectID, Name: w.Name, Kind: w.Kind, Image: w.Image,
			TargetPort: w.TargetPort, MemoryLimit: w.MemoryLimit, CPULimit: w.CPULimit, HealthPath: w.LivenessPath}
	}
	var st statusJSON
	if !status.IsError && json.Unmarshal([]byte(status.Content), &st) == nil {
		in.State.Status = st.Status
		if st.Kind != "" {
			in.Workload.Kind = st.Kind
		}
		for _, p := range st.Pods {
			in.State.Restarts += p.RestartCount
			if t := p.LastTermination; t != nil && !in.State.HasExitCode {
				in.State.LastReason, in.State.LastExitCode, in.State.HasExitCode = t.Reason, t.ExitCode, true
			}
		}
		if pf := st.PullFailure; pf != nil {
			in.State.PullCause, in.State.PullRegistry = pf.Cause, pf.Registry
		}
	}
	var evs []struct {
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Count   int    `json:"count"`
	}
	if !events.IsError && json.Unmarshal([]byte(events.Content), &evs) == nil {
		for _, e := range evs {
			in.Events = append(in.Events, diagnosis.Event{Type: e.Type, Reason: e.Reason, Message: e.Message, Count: e.Count})
		}
	}
	if !logs.IsError {
		in.Logs = logs.Content
	}
	if previous != nil && !previous.IsError {
		in.PreviousLogs = previous.Content
	}
	return in
}

// neighborhood : variables du conteneur, services, bases et autres
// conteneurs du projet, tailles du catalogue — chacun par sa route, avec les
// droits de la personne. Un morceau absent reste « inconnu ».
func (c *Catalog) neighborhood(ctx context.Context, inv Invoker, in *diagnosis.Input) {
	read := func(tool string, args map[string]any, out any) bool {
		t, ok := c.tools[tool]
		if !ok {
			return false
		}
		r := c.callRoute(ctx, inv, t, args)
		return !r.IsError && !r.Truncated && json.Unmarshal([]byte(r.Content), out) == nil
	}
	w := &in.Workload
	if w.ID != "" {
		var vars struct {
			Variables []struct {
				Name   string `json:"name"`
				Secret bool   `json:"secret"`
				Value  string `json:"value"`
			} `json:"variables"`
		}
		if read(toolListWorkloadVariables, map[string]any{"id": w.ID}, &vars) {
			w.Variables = []diagnosis.Variable{}
			for _, v := range vars.Variables {
				dv := diagnosis.Variable{Name: v.Name, Secret: v.Secret}
				if !v.Secret {
					dv.Value = v.Value // jamais la valeur d'un secret (l'API ne la rend pas)
				}
				w.Variables = append(w.Variables, dv)
			}
		}
	}
	var sizes struct {
		Sizes []struct {
			ID       string `json:"id"`
			CPUMilli int64  `json:"cpu_milli"`
			MemMi    int64  `json:"memory_mi"`
		} `json:"sizes"`
	}
	if read(toolGetPriceCatalog, map[string]any{}, &sizes) {
		for _, s := range sizes.Sizes {
			in.Sizes = append(in.Sizes, diagnosis.Size{ID: s.ID, CPUMilli: s.CPUMilli, MemMi: s.MemMi})
		}
	}
	if w.ProjectID == "" {
		return
	}
	var addons struct {
		Addons []serviceJSON `json:"addons"`
	}
	if in.AddonsKnown = read(toolListAddons, map[string]any{"id": w.ProjectID}, &addons); in.AddonsKnown {
		for _, a := range addons.Addons {
			in.Addons = append(in.Addons, diagnosis.Addon{ID: a.ID, Name: a.Name, Template: a.Template, Status: a.Status,
				Host: a.Host, Port: a.Port, Attached: attachments(a.Attachments)})
		}
	}
	var dbs []serviceJSON
	if in.DatabasesKnown = read(toolListDatabases, map[string]any{"id": w.ProjectID}, &dbs); in.DatabasesKnown {
		for _, d := range dbs {
			in.Databases = append(in.Databases, diagnosis.Database{ID: d.ID, Name: d.Name, Status: d.Status,
				Host: d.Host, Port: d.Port, Attached: attachments(d.Attachments)})
		}
	}
	var others []workloadJSON
	if in.NeighborsKnown = read(toolListWorkloads, map[string]any{"project_id": w.ProjectID}, &others); in.NeighborsKnown {
		for _, o := range others {
			if o.ID == w.ID {
				continue
			}
			in.Neighbors = append(in.Neighbors, diagnosis.Neighbor{Name: o.Name, Kind: o.Kind, Port: o.TargetPort, ManagedBy: o.ManagedBy})
		}
	}
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
