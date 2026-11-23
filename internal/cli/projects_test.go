package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/cli/config"
)

func TestCLI_Projects(t *testing.T) {
	const second = "22222222-2222-2222-2222-222222222222"
	var created, renamed map[string]any
	const third = "33333333-3333-3333-3333-333333333333"
	polls := 0 // lectures après création : PROVISIONING puis READY
	savedPoll := projectPollEvery
	projectPollEvery = time.Millisecond
	t.Cleanup(func() { projectPollEvery = savedPoll })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/kmanager/v1")
		switch {
		case r.Method == "GET" && p == "/projects":
			projects := []any{
				map[string]any{"project_id": projectID, "name": "Bomba", "status": "ACTIVE", "organization_id": "org-1"},
				map[string]any{"project_id": second, "name": "Bomba préprod", "status": "ACTIVE", "organization_id": "org-1"},
			}
			if created != nil {
				polls++
				status := "PROVISIONING"
				if polls >= 2 {
					status = "READY"
				}
				projects = append(projects, map[string]any{"project_id": third, "name": "Bomba staging", "status": status, "organization_id": "org-1"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects":      projects,
				"organizations": []any{map[string]any{"organization_id": "org-1", "display_name": "Awa", "projects": 2, "max_projects": 3}},
			})
		case r.Method == "POST" && p == "/projects":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"project_id": "33333333-3333-3333-3333-333333333333", "status": "PENDING"})
		case r.Method == "PATCH" && p == "/projects/"+second:
			_ = json.NewDecoder(r.Body).Decode(&renamed)
			_ = json.NewEncoder(w).Encode(map[string]any{"project_id": second, "name": renamed["name"]})
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
	require.NoError(t, config.SaveSettings(func(s *config.Settings) { s.ProjectID = projectID }))

	out, err := run(t, "projects", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "*  Bomba ", "projet courant marqué")
	assert.Contains(t, out, "Awa : 2 projet(s) sur 3")

	// Par nom, sans tenir compte de la casse.
	out, err = run(t, "projects", "use", "bomba PRÉPROD")
	require.NoError(t, err)
	assert.Contains(t, out, "Projet courant : Bomba préprod")
	s, err := config.LoadSettings()
	require.NoError(t, err)
	assert.Equal(t, second, s.ProjectID)

	_, err = run(t, "projects", "use", "inconnu")
	assert.Contains(t, describe(err), "aucun projet « inconnu »")

	out, err = run(t, "projects", "create", "Bomba staging", "--org", "org-1")
	require.NoError(t, err)
	assert.Contains(t, out, "betorun projects use 33333333")
	assert.Equal(t, map[string]any{"name": "Bomba staging", "organization_id": "org-1"}, created)
	assert.Equal(t, 2, polls, "rend la main seulement une fois le projet prêt")

	out, err = run(t, "projects", "rename", second, "Bomba recette")
	require.NoError(t, err)
	assert.Contains(t, out, "renommé en « Bomba recette »")
	assert.Equal(t, map[string]any{"name": "Bomba recette"}, renamed, "description inchangée")
}
