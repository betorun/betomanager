package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const testSpec = `{"paths":{
 "/api/workloads/{id}":{"get":{"summary":"Container details","description":"Container details\n\n**Access**: x","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}]}},
 "/api/workloads/{id}/status":{"get":{"summary":"Live state","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}]}},
 "/api/workloads/{id}/events":{"get":{"summary":"Events","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}]}},
 "/api/workloads/{id}/logs":{"get":{"summary":"Logs","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}},
   {"name":"follow","in":"query","schema":{"type":"boolean"}},{"name":"tail","in":"query","schema":{"type":"integer"}},{"name":"previous","in":"query","schema":{"type":"boolean"}}]}},
 "/api/workloads":{"get":{"summary":"List","parameters":[{"name":"project_id","in":"query","description":"Project","schema":{"type":"string"}}]}}
}}`

var testRoutes = []Route{
	{Name: ToolGetWorkload, Method: "GET", Path: "/api/workloads/:id"},
	{Name: ToolGetWorkloadStatus, Method: "GET", Path: "/api/workloads/:id/status"},
	{Name: ToolGetWorkloadEvents, Method: "GET", Path: "/api/workloads/:id/events"},
	{Name: ToolGetWorkloadLogs, Method: "GET", Path: "/api/workloads/:id/logs", Fixed: map[string]string{"follow": "false"}, Max: map[string]int{"tail": 300}},
	{Name: "list_workloads", Method: "GET", Path: "/api/workloads"},
}

// fakeAPI : enregistre les requêtes et répond selon le chemin.
type fakeAPI struct {
	reqs     []*http.Request
	restarts int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.reqs = append(f.reqs, r)
	switch {
	case r.Header.Get("Authorization") != "Bearer tok":
		w.WriteHeader(401)
	case strings.HasSuffix(r.URL.Path, "/logs"):
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Query().Get("previous") == "true" {
			fmt.Fprint(w, "panic: nil map\n")
			return
		}
		for i := 0; i < 3000; i++ {
			fmt.Fprintf(w, "line %d\n", i)
		}
		fmt.Fprint(w, "Error: listen EADDRINUSE :3000\n")
	case strings.HasSuffix(r.URL.Path, "/status"):
		fmt.Fprintf(w, `{"status":"FAILED","pods":[{"restart_count":%d}]}`, f.restarts)
	case strings.HasSuffix(r.URL.Path, "/events"):
		fmt.Fprint(w, `[{"reason":"BackOff"}]`)
	case strings.HasPrefix(r.URL.Path, "/api/workloads/forbidden"):
		w.WriteHeader(403)
		fmt.Fprint(w, `{"code":"PERMISSION_DENIED","message":"permission denied"}`)
	default:
		fmt.Fprint(w, `{"id":"w1",  "name":"web"}`)
	}
}

func catalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Build(testRoutes, []byte(testSpec))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddDiagnose(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBuildRejectsWritesAndUnknownRoutes(t *testing.T) {
	if _, err := Build([]Route{{Name: "delete_it", Method: "DELETE", Path: "/api/workloads/:id"}}, []byte(testSpec)); err == nil {
		t.Fatal("écriture acceptée comme outil")
	}
	if _, err := Build([]Route{{Name: "ghost", Method: "GET", Path: "/api/nope"}}, []byte(testSpec)); err == nil {
		t.Fatal("route absente de la spec acceptée")
	}
	c := catalog(t)
	tl, _ := c.Lookup(ToolGetWorkload)
	if tl.Description != "Container details." {
		t.Fatalf("description = %q (la mention d'accès ne doit pas y être)", tl.Description)
	}
	var schema map[string]any
	_ = json.Unmarshal(tl.Schema, &schema)
	if schema["required"] == nil {
		t.Fatal("required ne doit jamais être null")
	}
}

func TestCallRoute(t *testing.T) {
	c, api := catalog(t), &fakeAPI{}
	inv := Invoker{Handler: api, Token: "tok", ClientIP: "203.0.113.7", AcceptLanguage: "fr"}
	ctx := context.Background()

	r := c.Call(ctx, inv, ToolGetWorkload, json.RawMessage(`{"id":"w1"}`))
	if r.IsError || r.Content != `{"id":"w1","name":"web"}` {
		t.Fatalf("résultat = %+v", r)
	}
	req := api.reqs[0]
	if req.URL.Path != "/api/workloads/w1" || req.RemoteAddr != "203.0.113.7:0" || req.Header.Get("Accept-Language") != "fr" {
		t.Fatalf("requête = %s %s %v", req.URL, req.RemoteAddr, req.Header)
	}

	// Paramètre imposé (follow=false), plafond (tail ≤ 300), fin des journaux gardée.
	r = c.Call(ctx, inv, ToolGetWorkloadLogs, json.RawMessage(`{"id":"w1","tail":5000}`))
	q := api.reqs[1].URL.Query()
	if q.Get("follow") != "false" || q.Get("tail") != "300" {
		t.Fatalf("query = %v", q)
	}
	if !r.Truncated || !strings.Contains(r.Content, "EADDRINUSE") || strings.Contains(r.Content, "line 0\n") {
		t.Fatal("journaux : la FIN doit être gardée")
	}

	for name, args := range map[string]string{
		"argument inconnu":   `{"id":"w1","evil":"x"}`,
		"follow imposé":      `{"id":"w1","follow":true}`,
		"chemin traversant":  `{"id":"../projects"}`,
		"identifiant absent": `{}`,
		"pas un objet":       `[1]`,
	} {
		tool := ToolGetWorkload
		if name == "follow imposé" {
			tool = ToolGetWorkloadLogs
		}
		if r := c.Call(ctx, inv, tool, json.RawMessage(args)); !r.IsError {
			t.Errorf("%s : erreur attendue, eu %+v", name, r)
		}
	}
	if r := c.Call(ctx, inv, "drop_database", nil); !r.IsError {
		t.Error("outil inconnu accepté")
	}

	// Refus de l'API : statut et message, sans le reste du corps.
	r = c.Call(ctx, inv, ToolGetWorkload, json.RawMessage(`{"id":"forbidden"}`))
	if !r.IsError || r.Status != 403 || !strings.Contains(r.Content, "PERMISSION_DENIED") {
		t.Fatalf("refus = %+v", r)
	}
}

func TestDiagnose(t *testing.T) {
	c, api := catalog(t), &fakeAPI{restarts: 4}
	inv := Invoker{Handler: api, Token: "tok"}
	r := c.Call(context.Background(), inv, ToolDiagnoseWorkload, json.RawMessage(`{"id":"w1"}`))
	if r.IsError {
		t.Fatalf("diagnostic : %+v", r)
	}
	for _, want := range []string{"## workload", "## status", "## events", "BackOff", "## logs", "EADDRINUSE", "## previous_logs", "panic: nil map"} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("diagnostic sans %q", want)
		}
	}
	// Sans redémarrage : pas de journaux précédents.
	api2 := &fakeAPI{}
	r = c.Call(context.Background(), Invoker{Handler: api2, Token: "tok"}, ToolDiagnoseWorkload, json.RawMessage(`{"id":"w1"}`))
	if strings.Contains(r.Content, "previous_logs") {
		t.Error("journaux précédents demandés sans redémarrage")
	}
	// Conteneur refusé : le diagnostic s'arrête net.
	r = c.Call(context.Background(), inv, ToolDiagnoseWorkload, json.RawMessage(`{"id":"forbidden"}`))
	if !r.IsError {
		t.Fatal("diagnostic d'un conteneur refusé : erreur attendue")
	}
}

// Diagnostic déterministe : constats en tête, voisinage lu par les routes,
// correction gardée seulement si l'outil visé l'accepte (Prepare).
func TestDiagnoseFindings(t *testing.T) {
	spec := strings.Replace(testSpec, `"/api/workloads":{`, `"/api/projects/{id}/addons":{"get":{"summary":"Addons","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}]}},
 "/api/projects/{id}/addons/{addonID}/attach":{"post":{"summary":"Attach","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}},{"name":"addonID","in":"path","required":true,"schema":{"type":"string"}}],
   "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"workload_id":{"type":"string"},"variable":{"type":"string"}}}}}}}},
 "/api/workloads":{`, 1)
	routes := append(append([]Route(nil), testRoutes...),
		Route{Name: "list_addons", Method: "GET", Path: "/api/projects/:id/addons"},
		Route{Name: "attach_addon", Method: "POST", Path: "/api/projects/:id/addons/:addonID/attach", Write: true, Risk: RiskMedium})
	c, err := Build(routes, []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddDiagnose(c); err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			fmt.Fprint(w, "Example app listening on port 3000\nError: connect ECONNREFUSED 127.0.0.1:6379\n")
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"status":"CRASHLOOP","kind":"web","pods":[{"restart_count":0}]}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/addons"):
			fmt.Fprint(w, `{"addons":[{"id":"a1","name":"cache","template":"redis","status":"RUNNING","host":"cache","port":6379,"attachments":[]}]}`)
		default:
			fmt.Fprint(w, `{"ID":"w1","ProjectID":"p1","Name":"api","Kind":"web","TargetPort":8080,"MemoryLimit":"256Mi"}`)
		}
	})
	r := c.Call(context.Background(), Invoker{Handler: api, Token: "tok"}, ToolDiagnoseWorkload, json.RawMessage(`{"id":"w1"}`))
	if r.IsError || !strings.HasPrefix(r.Content, "## findings\n[") {
		t.Fatalf("constats en tête : %s", r.Content)
	}
	head := strings.SplitN(r.Content, "\n\n", 2)[0]
	var fs []struct {
		Code     string            `json:"code"`
		Evidence []string          `json:"evidence"`
		Params   map[string]string `json:"params"`
		Fix      *struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		} `json:"fix"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(head, "## findings\n")), &fs); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for i, f := range fs {
		got[f.Code] = i
	}
	dep, port := fs[got["localhost_dependency"]], fs[got["port_mismatch"]]
	if dep.Fix == nil || dep.Fix.Tool != "attach_addon" || dep.Fix.Args["addonID"] != "a1" || dep.Fix.Args["workload_id"] != "w1" || dep.Params["internal_address"] != "cache:6379" {
		t.Fatalf("dépendance : %+v", dep)
	}
	// update_workload absent de ce catalogue : le constat reste, sans correction.
	if port.Code != "port_mismatch" || port.Fix != nil {
		t.Fatalf("port : %+v", port)
	}
	for _, want := range []string{"## workload", "## status", "## logs"} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("sans %q", want)
		}
	}
}
