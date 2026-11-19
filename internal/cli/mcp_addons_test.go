package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const tAddon = "44444444-4444-4444-4444-444444444444"

// fakeAddonAPI : routes des services et des conteneurs pour les outils MCP.
type fakeAddonAPI struct {
	mu       sync.Mutex
	polls    int
	created  map[string]any
	attached map[string]any
}

func (f *fakeAddonAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	base := "/projects/" + tProject + "/addons"
	addon := func(status string) string {
		return fmt.Sprintf(`{"id":%q,"name":"files","title":"Redis (via Valkey)","version":"9.1","status":%q,"host":"files:6379","variable":"REDIS_URL"}`, tAddon, status)
	}
	switch {
	case p == "/workloads" && r.Method == "GET":
		fmt.Fprintf(w, `[{"ID":%q,"Name":"app","Status":"RUNNING"},{"ID":"55555555-5555-5555-5555-555555555555","Name":"files","Status":"RUNNING","ManagedBy":"addon/redis/x"}]`, tWorkload)
	case p == base && r.Method == "POST":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		fmt.Fprint(w, addon("STARTING"))
	case p == base && r.Method == "GET":
		fmt.Fprintf(w, `{"addons":[%s],"max":10}`, addon("RUNNING"))
	case p == base+"/"+tAddon:
		f.polls++
		status := "STARTING"
		if f.polls >= 2 {
			status = "RUNNING"
		}
		fmt.Fprint(w, addon(status))
	case p == base+"/"+tAddon+"/attach":
		_ = json.NewDecoder(r.Body).Decode(&f.attached)
		fmt.Fprint(w, `{"variable":"REDIS_URL","redeploy":true}`)
	default:
		w.WriteHeader(404)
		fmt.Fprintf(w, `{"error":"not found %s %s"}`, r.Method, p)
	}
}

func addonEnv(t *testing.T, f *fakeAddonAPI) mcpEnv {
	t.Helper()
	srv := httptest.NewServer(f)
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

func TestCreateAddonAttachesAndWaits(t *testing.T) {
	f := &fakeAddonAPI{}
	e := addonEnv(t, f)
	res := callTool(t, e, "create_addon", fmt.Sprintf(`{"name":"files","project":%q,"mode":"cache","attach_to":"app"}`, tProject))
	if res.IsError {
		t.Fatalf("création : %s", res.Text)
	}
	if f.created["template"] != "redis" || f.created["mode"] != "cache" {
		t.Fatalf("demande : %v", f.created)
	}
	if f.attached["workload_id"] != tWorkload {
		t.Fatalf("branchement : %v", f.attached)
	}
	for _, want := range []string{"is ready", "files:6379", "REDIS_URL"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("texte sans %q : %s", want, res.Text)
		}
	}
}

func TestAttachAddonRefusesServiceCarrier(t *testing.T) {
	e := addonEnv(t, &fakeAddonAPI{})
	res := callTool(t, e, "attach_addon", fmt.Sprintf(`{"addon":"files","workload":"files","project":%q}`, tProject))
	if !res.IsError || !strings.Contains(res.Text, "runs a service") {
		t.Fatalf("porteur accepté : %+v", res)
	}
	res = callTool(t, e, "list_addons", fmt.Sprintf(`{"project":%q}`, tProject))
	if res.IsError || !strings.Contains(res.Text, "files:6379") {
		t.Fatalf("liste : %+v", res)
	}
}
