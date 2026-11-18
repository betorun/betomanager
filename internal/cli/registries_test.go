package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRegistriesAPI(t *testing.T, put *map[string]string, testResult string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /projects/{id}/registries/{host}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(put)
		_ = json.NewEncoder(w).Encode(map[string]any{"host": r.PathValue("host"), "username": (*put)["username"]})
	})
	mux.HandleFunc("GET /projects/{id}/registries", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"registries": []map[string]any{{"host": "ghcr.io", "username": "moi"}}})
	})
	mux.HandleFunc("POST /projects/{id}/registries/{host}/test", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": testResult})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("BETORUN_CONFIG_DIR", t.TempDir())
	t.Setenv("BETORUN_LANG", "fr")
	t.Setenv("BETORUN_TOKEN", "ci-token")
	t.Setenv("BETORUN_API_URL", srv.URL)
	t.Setenv("BETORUN_PROJECT", projectID)
}

// Le jeton est demandé, puis envoyé dans le corps de la requête seulement.
func TestRegistriesAdd_PromptsForToken(t *testing.T) {
	put := map[string]string{}
	fakeRegistriesAPI(t, &put, "ok")
	saved := readCode
	defer func() { readCode = saved }()
	var asked []string
	readCode = func(prompt string) (string, error) { asked = append(asked, prompt); return "ghp_tapé", nil }

	out, err := run(t, "registries", "add", "ghcr.io", "--user", "moi")
	require.NoError(t, err)
	require.Len(t, asked, 1)
	assert.Contains(t, asked[0], "ghcr.io")
	assert.Equal(t, map[string]string{"username": "moi", "password": "ghp_tapé"}, put)
	assert.NotContains(t, out, "ghp_tapé", "le jeton n'est jamais affiché")

	_, err = run(t, "registries", "add", "ghcr.io")
	require.Error(t, err, "--user obligatoire")
}

func TestRegistriesTest_Denied(t *testing.T) {
	put := map[string]string{}
	fakeRegistriesAPI(t, &put, "denied")
	_, err := run(t, "registries", "test", "ghcr.io")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refuse")
}
