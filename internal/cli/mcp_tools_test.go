package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/mcp"
)

const (
	tProject  = "11111111-1111-1111-1111-111111111111"
	tBuild    = "22222222-2222-2222-2222-222222222222"
	tWorkload = "33333333-3333-3333-3333-333333333333"
)

// fakeBetorun : API minimale pour le parcours de déploiement.
type fakeBetorun struct {
	mu          sync.Mutex
	buildPolls  int
	buildFails  bool
	existing    bool
	kind        string // sorte du conteneur existant
	created     map[string]any
	updated     map[string]any
	uploadNames []string
}

func (f *fakeBetorun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/projects" && r.Method == "GET":
		fmt.Fprintf(w, `{"projects":[{"project_id":%q,"name":"demo","status":"READY"}]}`, tProject)
	case strings.HasSuffix(p, "/builds/upload"):
		_ = r.ParseMultipartForm(1 << 20)
		f.uploadNames = append(f.uploadNames, r.FormValue("name"))
		fmt.Fprintf(w, `{"id":%q,"project_id":%q,"name":"app","status":"QUEUED"}`, tBuild, tProject)
	case strings.HasSuffix(p, "/builds/"+tBuild+"/logs"):
		fmt.Fprint(w, "step 1\nnpm ERR! missing script: build\n")
	case strings.HasSuffix(p, "/builds/"+tBuild):
		f.buildPolls++
		status := "BUILDING"
		if f.buildPolls >= 2 {
			status = "SUCCEEDED"
			if f.buildFails {
				status = "FAILED"
			}
		}
		fmt.Fprintf(w, `{"id":%q,"project_id":%q,"name":"app","status":%q,"image":"registry/app:1","detected":"next","failure":"BUILD_FAILED"}`, tBuild, tProject, status)
	case p == "/workloads" && r.Method == "GET":
		if f.existing {
			fmt.Fprintf(w, `[{"id":%q,"name":"app","status":"RUNNING","Kind":%q}]`, tWorkload, f.kind)
		} else {
			fmt.Fprint(w, `[]`)
		}
	case p == "/workloads" && r.Method == "POST":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		fmt.Fprintf(w, `{"workload_id":%q,"status":"STARTING"}`, tWorkload)
	case p == "/workloads/"+tWorkload+"/variables":
		fmt.Fprint(w, `{"variables":[{"name":"KEEP","value":"1"},{"name":"SECRET","secret":true}]}`)
	case p == "/workloads/"+tWorkload && r.Method == "PATCH":
		_ = json.NewDecoder(r.Body).Decode(&f.updated)
		fmt.Fprint(w, `{"workload_id":"x"}`)
	case p == "/workloads/"+tWorkload:
		fmt.Fprintf(w, `{"id":%q,"name":"app","status":"RUNNING","ExternalURL":"https://app-demo.example"}`, tWorkload)
	default:
		w.WriteHeader(404)
		fmt.Fprintf(w, `{"error":"not found %s %s"}`, r.Method, p)
	}
}

func testEnv(t *testing.T, api *fakeBetorun) mcpEnv {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return mcpEnv{
		settings: func() (settingsLike, error) { return settingsLike{APIURL: srv.URL}, nil },
		client: func(u string) *apiClient {
			c := newAPIClient(u)
			c.Source = staticToken("tok")
			return c
		},
		now: time.Now, buildTimeout: time.Minute, workloadTimeout: time.Minute, poll: time.Millisecond,
	}
}

func project(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "my-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"package.json": `{"name":"app"}`, ".env": "SECRET=1", "index.js": "x"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func callTool(t *testing.T, e mcpEnv, tool string, args string) mcp.Result {
	t.Helper()
	for _, tl := range e.tools() {
		if tl.Name == tool {
			res, err := tl.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(args), Progress: func(float64, float64, string) {}})
			if err != nil {
				return mcp.Result{Text: err.Error(), IsError: true}
			}
			return res
		}
	}
	t.Fatalf("outil %s absent", tool)
	return mcp.Result{}
}

func TestDeployCreatesAndReturnsURL(t *testing.T) {
	f := &fakeBetorun{}
	e := testEnv(t, f)
	res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q,"name":"app","env":{"NODE_ENV":"production"}}`, project(t)))
	if res.IsError {
		t.Fatalf("déploiement : %s", res.Text)
	}
	d := res.Structured.(DeployResult)
	if d.Status != "running" || d.URL != "https://app-demo.example" || d.Detected != "next" || d.Image != "registry/app:1" {
		t.Fatalf("résultat : %+v", d)
	}
	if f.created["image"] != "registry/app:1" || f.created["target_port"] != float64(8080) || f.created["project_id"] != tProject {
		t.Fatalf("création : %v", f.created)
	}
}

func TestDeployUpdatesKeepsVariables(t *testing.T) {
	f := &fakeBetorun{existing: true}
	e := testEnv(t, f)
	res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q,"name":"app","env":{"NEW":"2"}}`, project(t)))
	if res.IsError {
		t.Fatalf("déploiement : %s", res.Text)
	}
	env, _ := f.updated["env_vars"].(map[string]any)
	if env["KEEP"] != "1" || env["NEW"] != "2" || env["SECRET"] != nil {
		t.Fatalf("variables : %v (existantes gardées, secrets jamais réécrits)", f.updated["env_vars"])
	}
}

func TestDeployBuildFailureReturnsLog(t *testing.T) {
	f := &fakeBetorun{buildFails: true}
	e := testEnv(t, f)
	res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q}`, project(t)))
	if !res.IsError || !strings.Contains(res.Text, "missing script: build") || res.Structured.(DeployResult).Status != "build_failed" {
		t.Fatalf("échec du build : %s", res.Text)
	}
	if f.created != nil {
		t.Fatal("aucun conteneur ne doit être créé après un build échoué")
	}
	if f.uploadNames[0] != "my-app" {
		t.Fatalf("nom par défaut = %q (nom du dossier attendu)", f.uploadNames[0])
	}
}

func TestDeployRefusesBadInput(t *testing.T) {
	e := testEnv(t, &fakeBetorun{})
	home, _ := os.UserHomeDir()
	for name, args := range map[string]string{
		"racine":          `{"path":"/"}`,
		"dossier perso":   fmt.Sprintf(`{"path":%q}`, home),
		"pas un dossier":  `{"path":"/nonexistent/xyz"}`,
		"nom invalide":    fmt.Sprintf(`{"path":%q,"name":"My App!"}`, project(t)),
		"env non textuel": fmt.Sprintf(`{"path":%q,"env":{"A":1}}`, project(t)),
		"chemin absent":   `{}`,
	} {
		if res := callTool(t, e, "deploy", args); !res.IsError {
			t.Errorf("%s : refus attendu, eu %s", name, res.Text)
		}
	}
}

func TestDeployCronWithoutPort(t *testing.T) {
	f := &fakeBetorun{}
	e := testEnv(t, f)
	res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q,"name":"app","kind":"cron","schedule":"0 2 * * *","time_zone":"Africa/Dakar","command":["node","job.js"]}`, project(t)))
	if res.IsError {
		t.Fatalf("déploiement : %s", res.Text)
	}
	if f.created["kind"] != "cron" || f.created["schedule"] != "0 2 * * *" || f.created["time_zone"] != "Africa/Dakar" || f.created["target_port"] != float64(0) {
		t.Fatalf("création : %v", f.created)
	}
	if cmd, _ := f.created["command"].([]any); len(cmd) != 2 || cmd[1] != "job.js" {
		t.Fatalf("commande : %v", f.created["command"])
	}
	if !strings.Contains(res.Text, "schedule") {
		t.Fatalf("résumé : %s", res.Text)
	}
}

func TestDeployKindChecksBeforeBuild(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *fakeBetorun
		args string
	}{
		"cron sans horaire":      {&fakeBetorun{}, `"kind":"cron"`},
		"horaire hors cron":      {&fakeBetorun{}, `"schedule":"@daily"`},
		"sorte inconnue":         {&fakeBetorun{}, `"kind":"batch"`},
		"sorte qui change":       {&fakeBetorun{existing: true, kind: "web"}, `"kind":"worker"`},
		"commande non textuelle": {&fakeBetorun{}, `"command":"node job.js"`},
	} {
		e := testEnv(t, tc.f)
		res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q,"name":"app",%s}`, project(t), tc.args))
		if !res.IsError {
			t.Errorf("%s : refus attendu, eu %s", name, res.Text)
		}
		if len(tc.f.uploadNames) > 0 {
			t.Errorf("%s : aucun build ne doit partir", name)
		}
	}
}

func TestDeployExistingWorkerKeepsKind(t *testing.T) {
	f := &fakeBetorun{existing: true, kind: "worker"}
	e := testEnv(t, f)
	res := callTool(t, e, "deploy", fmt.Sprintf(`{"path":%q,"name":"app"}`, project(t)))
	if res.IsError {
		t.Fatalf("déploiement : %s", res.Text)
	}
	if f.updated["target_port"] != nil && f.updated["target_port"] != float64(0) {
		t.Fatalf("un worker ne reçoit pas de port : %v", f.updated)
	}
	if res.Structured.(DeployResult).Kind != "worker" {
		t.Fatalf("sorte : %+v", res.Structured)
	}
}

func TestNotSignedIn(t *testing.T) {
	f := &fakeBetorun{}
	e := testEnv(t, f)
	e.client = func(u string) *apiClient { c := newAPIClient(u); c.Source = staticToken("bad"); return c }
	if res := callTool(t, e, "list_projects", `{}`); !res.IsError || !strings.Contains(res.Text, "betorun login") {
		t.Fatalf("sans session : %s", res.Text)
	}
}

// Aliases de test (évite de répéter le paquet api partout).
type apiClient = api.Client

func newAPIClient(u string) *api.Client { return api.New(u) }

func staticToken(t string) api.TokenSource { return api.StaticSource(t) }
