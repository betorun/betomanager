package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Suppression : nom retapé, échéance affichée ; restauration ; un mauvais
// nom n'envoie rien.
func TestCLI_ProjectsDeleteAndRestore(t *testing.T) {
	deletes, restores := 0, 0
	scheduled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/kmanager/v1")
		switch {
		case r.Method == "GET" && p == "/projects":
			pr := map[string]any{"project_id": projectID, "name": "Boutique", "status": "READY", "organization_id": "org-1"}
			if scheduled {
				pr["status"], pr["deletion_scheduled_at"] = "PENDING_DELETION", "2026-11-24T22:00:00Z"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{pr}, "organizations": []any{}})
		case r.Method == "DELETE" && p == "/projects/"+projectID:
			deletes++
			scheduled = true
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"project_id":"` + projectID + `","status":"PENDING_DELETION","deletion_scheduled_at":"2026-11-24T22:00:00Z"}`))
		case r.Method == "POST" && p == "/projects/"+projectID+"/restore":
			restores++
			scheduled = false
			_, _ = w.Write([]byte(`{"project_id":"` + projectID + `","name":"Boutique","status":"READY"}`))
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

	_, err := runIn(t, "Boutik\n", "projects", "delete", "Boutique")
	assert.Contains(t, describe(err), "le nom ne correspond pas")
	assert.Zero(t, deletes, "mauvais nom : rien d'envoyé")

	out, err := runIn(t, "Boutique\n", "projects", "delete", "Boutique")
	require.NoError(t, err)
	assert.Equal(t, 1, deletes)
	assert.Contains(t, out, "effacé définitivement dans 12 h")
	assert.Contains(t, out, "effacement définitif le")
	assert.Contains(t, out, `betorun projects restore "Boutique"`)

	out, err = run(t, "projects", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "suppression le")

	out, err = run(t, "projects", "restore", "Boutique")
	require.NoError(t, err)
	assert.Equal(t, 1, restores)
	assert.Contains(t, out, "restauré")
}

// runIn : comme run, avec une saisie au clavier.
func runIn(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	root := newRoot("betorun")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs(args)
	flagAPI, flagProject, flagJSON = "", "", false
	err := root.Execute()
	return out.String(), err
}
