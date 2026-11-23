package domain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Le script et celui qu'il écrit (heredoc) sont du sh valide.
func TestSMTPBlockScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh absent")
	}
	dir := t.TempDir()
	outer := filepath.Join(dir, "install.sh")
	require.NoError(t, os.WriteFile(outer, []byte(SMTPBlockScript), 0o600))
	out, err := exec.Command("sh", "-n", outer).CombinedOutput()
	require.NoError(t, err, string(out))

	inner := SMTPBlockScript[strings.Index(SMTPBlockScript, "<<'BETORUN'\n")+len("<<'BETORUN'\n"):]
	inner = inner[:strings.Index(inner, "BETORUN\n")]
	path := filepath.Join(dir, "block.sh")
	require.NoError(t, os.WriteFile(path, []byte(inner), 0o600))
	out, err = exec.Command("sh", "-n", path).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, inner, "tcp dport 25 drop")
	assert.Contains(t, inner, "--dport 25 -j DROP", "repli iptables")
}

func TestValidReverse(t *testing.T) {
	for _, ok := range []string{"mail.exemple.cg", "MAIL.exemple.cg.", "a.b.c"} {
		assert.True(t, ValidReverse(ok), ok)
	}
	for _, bad := range []string{"", "localhost", "-a.b", "a_b.c", "a..b"} {
		assert.False(t, ValidReverse(bad), bad)
	}
}
