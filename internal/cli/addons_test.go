package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAddons : routes services, conteneurs et « Me prévenir ».
type fakeAddons struct {
	mu        sync.Mutex
	addons    []map[string]any
	created   map[string]any
	attached  []map[string]any
	upgraded  []map[string]any
	deleted   bool
	waitlist  []string
	statusGet int
}

func newFakeAddons(t *testing.T) (*httptest.Server, *fakeAddons) {
	f := &fakeAddons{}
	apiID, carrierID := uuid.NewString(), uuid.NewString()
	addonID := uuid.NewString()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	base := "/projects/" + projectID + "/addons"
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/kmanager/v1")
		switch {
		case r.Method == "GET" && p == base+"/templates":
			write(w, 200, map[string]any{
				"templates": []any{map[string]any{"id": "redis", "title": "Redis (via Valkey)", "logo": "valkey", "category": "cache",
					"port": 6379, "variable": "REDIS_URL", "recommended_version": "9.1",
					"versions": []any{map[string]any{"id": "9.1", "major": 9}, map[string]any{"id": "8.1", "major": 8}},
					"modes":    []any{map[string]any{"id": "persistent", "persistent": true}, map[string]any{"id": "cache"}},
					"disks":    []string{"1Gi", "2Gi", "5Gi"}}},
				"sizes":    []any{map[string]any{"id": "mini", "cpu_request": "50m", "cpu_limit": "250m", "memory": "256Mi"}},
				"upcoming": []any{map[string]any{"id": "mariadb", "title": "MariaDB", "logo": "mariadb", "category": "databases"}},
			})
		case r.Method == "GET" && p == "/workloads":
			write(w, 200, []any{
				map[string]any{"ID": apiID, "Name": "api", "Status": "RUNNING"},
				map[string]any{"ID": carrierID, "Name": "files-taches", "Status": "RUNNING", "ManagedBy": "addon/redis/" + addonID},
			})
		case r.Method == "GET" && p == base:
			write(w, 200, map[string]any{"addons": f.addons, "max": 10})
		case r.Method == "POST" && p == base:
			_ = json.NewDecoder(r.Body).Decode(&f.created)
			a := map[string]any{"id": addonID, "name": f.created["name"], "template": "redis", "title": "Redis (via Valkey)",
				"version": "8.1", "latest_version": "9.1", "upgrade_available": true, "size": "small", "disk": "1Gi",
				"status": "STARTING", "workload_id": carrierID, "host": f.created["name"].(string) + ":6379", "port": 6379,
				"variable": "REDIS_URL", "attachments": []any{}}
			f.addons = append(f.addons, a)
			write(w, 201, a)
		case r.Method == "GET" && p == base+"/"+addonID:
			if f.statusGet++; f.statusGet >= 2 {
				f.addons[0]["status"] = "RUNNING"
			}
			write(w, 200, f.addons[0])
		case r.Method == "POST" && p == base+"/"+addonID+"/attach":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.attached = append(f.attached, body)
			write(w, 200, map[string]any{"workload_id": body["workload_id"], "variable": "REDIS_URL", "redeploy": true})
		case r.Method == "POST" && p == base+"/"+addonID+"/upgrade":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.upgraded = append(f.upgraded, body)
			write(w, 200, f.addons[0])
		case r.Method == "DELETE" && p == base+"/"+addonID:
			f.deleted = true
			write(w, 200, map[string]any{"still_attached": []any{map[string]any{"workload_id": apiID, "workload_name": "api", "variable": "REDIS_URL"}}})
		case r.Method == "PUT" && p == "/addons/waitlist/mariadb":
			f.waitlist = []string{"mariadb"}
			write(w, 200, map[string]any{"templates": f.waitlist})
		case r.Method == "DELETE" && p == "/addons/waitlist/mariadb":
			f.waitlist = nil
			write(w, 200, map[string]any{"templates": []string{}})
		default:
			t.Errorf("route inattendue %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	})
	return srv, f
}

func TestCLI_Addons(t *testing.T) {
	srv, f := newFakeAddons(t)
	t.Setenv("BETORUN_CONFIG_DIR", t.TempDir())
	t.Setenv("BETORUN_LANG", "fr")
	t.Setenv("BETORUN_API_URL", srv.URL+"/kmanager/v1")
	t.Setenv("BETORUN_PROJECT", projectID)
	t.Setenv("BETORUN_TOKEN", "ci-token")
	workloadPollEvery = 5 * time.Millisecond

	out, err := run(t, "addons", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "Aucun service")

	out, err = run(t, "addons", "catalog")
	require.NoError(t, err)
	assert.Contains(t, out, "Redis (via Valkey)")
	assert.Contains(t, out, "9.1 (recommandée), 8.1")
	assert.Contains(t, out, "Bientôt : mariadb")

	// Conteneur inconnu ou porteur d'un service : refusé AVANT la création.
	_, err = run(t, "addons", "create", "redis", "cache", "--attach", "inconnu")
	assert.Contains(t, describe(err), `aucun conteneur nommé "inconnu"`)
	_, err = run(t, "addons", "create", "redis", "cache", "--attach", "files-taches")
	assert.Contains(t, describe(err), "porte un service")
	assert.Nil(t, f.created, "rien de créé")

	out, err = run(t, "services", "create", "redis", "files", "--mode", "cache", "--size", "mini", "--version", "8.1", "--attach", "api")
	require.NoError(t, err, out)
	assert.Equal(t, map[string]any{"template": "redis", "name": "files", "version": "8.1", "mode": "cache", "size": "mini", "disk": ""}, f.created)
	require.Len(t, f.attached, 1)
	assert.NotContains(t, f.attached[0], "variable", "variable par défaut du service")
	assert.Contains(t, out, "Variable REDIS_URL posée sur api")
	assert.Contains(t, out, "files est prêt.")
	assert.Contains(t, out, "Adresse dans le projet : files:6379")

	out, err = run(t, "addons", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "Redis (via Valkey) 8.1 (9.1 disponible)")
	assert.Contains(t, out, "Petit · 1Gi")
	assert.Contains(t, out, "1 / 10 services installés")

	_, err = run(t, "addons", "attach", "files", "api", "--variable", "QUEUE_URL")
	require.NoError(t, err)
	assert.Equal(t, "QUEUE_URL", f.attached[1]["variable"])

	// Changement de version majeure : à confirmer en retapant le nom.
	root := newRoot("betorun")
	root.SetIn(strings.NewReader("non\n"))
	root.SetOut(&strings.Builder{})
	root.SetArgs([]string{"addons", "upgrade", "files"})
	assert.Contains(t, describe(root.Execute()), "annulée")
	assert.Empty(t, f.upgraded)
	_, err = run(t, "addons", "upgrade", "files", "--yes")
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"version": "9.1"}}, f.upgraded)

	root = newRoot("betorun")
	root.SetIn(strings.NewReader("files\n"))
	var delOut strings.Builder
	root.SetOut(&delOut)
	root.SetArgs([]string{"addons", "delete", "files"})
	require.NoError(t, root.Execute())
	assert.True(t, f.deleted)
	assert.Contains(t, delOut.String(), "api (REDIS_URL)", "conteneurs encore branchés signalés")

	out, err = run(t, "addons", "notify", "mariadb")
	require.NoError(t, err)
	assert.Contains(t, out, "quand mariadb sera disponible")
	assert.Equal(t, []string{"mariadb"}, f.waitlist)
	_, err = run(t, "addons", "notify", "mariadb", "--off")
	require.NoError(t, err)
	assert.Empty(t, f.waitlist)
}
