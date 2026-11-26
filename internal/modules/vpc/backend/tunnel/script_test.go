package tunnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrollScript(t *testing.T) {
	b := &Backend{}
	token := "bvpc_" + strings.Repeat("a1_-", 10)
	s := b.EnrollScript("https://api.example.com/kmanager/v1/", token)
	require.NotEmpty(t, s)
	assert.Contains(t, s, "API='https://api.example.com/kmanager/v1'")
	assert.Contains(t, s, `TOKEN="${1:-`+token+`}"`)
	assert.Contains(t, s, "wg genkey", "clé générée sur le VPS")
	assert.NotContains(t, s, "__")

	served := b.EnrollScript("https://api.example.com/kmanager/v1", "")
	assert.Contains(t, served, `TOKEN="${1:-}"`)

	for _, bad := range []string{"", "ftp://x", "https://x/'; rm -rf /", "https://x/$(id)"} {
		assert.Empty(t, b.EnrollScript(bad, token), bad)
	}
	assert.Empty(t, b.EnrollScript("https://api.example.com", "bvpc_'$(id)'"))

	// Syntaxe POSIX vérifiée par sh -n.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh absent")
	}
	f := filepath.Join(t.TempDir(), "enroll.sh")
	require.NoError(t, os.WriteFile(f, []byte(s), 0o600))
	out, err := exec.Command(sh, "-n", f).CombinedOutput()
	assert.NoError(t, err, string(out))
}

// La réponse de l'API est lue par le script sans outil JSON : les clés
// rendues ne doivent pas se confondre.
func TestEnrollFieldsAreDistinct(t *testing.T) {
	keys := []string{"address", "endpoint", "peer_public_key", "allowed_ips", "keepalive"}
	for i, a := range keys {
		for j, c := range keys {
			if i != j {
				assert.NotContains(t, c, `"`+a+`"`)
			}
		}
	}
}
