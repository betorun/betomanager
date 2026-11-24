package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Régions listées, et la région choisie part avec la création du projet.
func TestCLI_RegionsAndCreateInRegion(t *testing.T) {
	var created map[string]any
	empty := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/kmanager/v1")
		switch {
		case r.Method == "GET" && p == "/regions":
			if empty {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"code":"af-central-dla","available":true,"default":true},{"code":"eu-west-gra","available":false,"default":false}]`))
		case r.Method == "POST" && p == "/projects":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"project_id":"` + projectID + `","status":"PROVISIONING"}`))
		default:
			t.Errorf("route inattendue %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BETORUN_CONFIG_DIR", t.TempDir())
	t.Setenv("BETORUN_LANG", "fr")
	t.Setenv("BETORUN_API_URL", srv.URL+"/kmanager/v1")
	t.Setenv("BETORUN_TOKEN", "ci-token")

	out, err := run(t, "regions")
	require.NoError(t, err)
	assert.Contains(t, out, "af-central-dla")
	assert.Contains(t, out, "par défaut")
	assert.Contains(t, out, "complète")

	_, err = run(t, "projects", "create", "Boutique Douala", "--region", " AF-Central-DLA ", "--no-wait")
	require.NoError(t, err)
	assert.Equal(t, "af-central-dla", created["region"])

	empty = true
	out, err = run(t, "regions")
	require.NoError(t, err)
	assert.Contains(t, out, "la plateforme place vos projets elle-même")
}
