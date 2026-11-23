package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanBytes(t *testing.T) {
	assert.Equal(t, "512 B", humanBytes(512))
	assert.Equal(t, "79.3 MiB", humanBytes(83177472))
	assert.Equal(t, "5.0 GiB", humanBytes(5368709120))
	assert.Equal(t, "—", usedPercent(1, 0))
	assert.Equal(t, "2.5 %", usedPercent(133812224, 5368709120))
}

// Usage réel : commande dédiée et colonne de « databases list » ; sans mesure,
// la liste s'affiche quand même.
func TestDiskUsage_CommandAndDatabaseColumn(t *testing.T) {
	measured := true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/{id}/disk-usage", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, projectID, r.PathValue("id"))
		if !measured {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"disks":[{"kind":"database","target_id":"3a6fd360-1623-4540-966f-b5d0ea48ee39","target_name":"main",` +
			`"capacity_bytes":5368709120,"used_bytes":133812224,"level":0,"measured_at":"2026-11-23T23:37:03Z"}]}`))
	})
	mux.HandleFunc("GET /projects/{id}/databases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"3a6fd360-1623-4540-966f-b5d0ea48ee39","name":"main","status":"READY","storage_size":"5Gi"}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("BETORUN_CONFIG_DIR", t.TempDir())
	t.Setenv("BETORUN_LANG", "fr")
	t.Setenv("BETORUN_TOKEN", "ci-token")
	t.Setenv("BETORUN_API_URL", srv.URL)
	t.Setenv("BETORUN_PROJECT", projectID)

	out, err := run(t, "projects", "disk-usage")
	require.NoError(t, err)
	assert.Contains(t, out, "main")
	assert.Contains(t, out, "127.6 MiB")
	assert.Contains(t, out, "5.0 GiB")
	assert.Contains(t, out, "2.5 %")

	out, err = run(t, "databases", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "127.6 MiB (2.5 %)")

	measured = false
	out, err = run(t, "databases", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "main")
	assert.Contains(t, out, "—")
}
