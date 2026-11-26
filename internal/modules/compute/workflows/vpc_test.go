package workflows

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

type fakeVPCHook struct {
	script string
	err    error
	asked  []uuid.UUID
}

func (f *fakeVPCHook) PrepareInstall(_ context.Context, id uuid.UUID) (string, error) {
	f.asked = append(f.asked, id)
	return f.script, f.err
}

func TestFirstBootScript(t *testing.T) {
	ctx := context.Background()
	v := &domain.VPS{ID: uuid.New()}

	a := &Activities{}
	got, err := a.firstBootScript(ctx, v)
	require.NoError(t, err)
	assert.Equal(t, domain.SMTPBlockScript, got, "sans réseau privé : comme avant")

	hook := &fakeVPCHook{script: "#!/bin/sh\nset -eu\necho vpc\nexit 1\n"}
	a.VPC = hook
	got, err = a.firstBootScript(ctx, v)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, domain.SMTPBlockScript), "port 25 d'abord")
	assert.Contains(t, got, "(\n#!/bin/sh\nset -eu\necho vpc\nexit 1\n\n)\n", "inscription en sous-shell")
	assert.Equal(t, []uuid.UUID{v.ID}, hook.asked)

	// Syntaxe POSIX du script complet.
	if sh, err := exec.LookPath("sh"); err == nil {
		f := filepath.Join(t.TempDir(), "boot.sh")
		require.NoError(t, os.WriteFile(f, []byte(got), 0o600))
		out, err := exec.Command(sh, "-n", f).CombinedOutput()
		assert.NoError(t, err, string(out))
	}

	v.SMTPAllowed = true
	got, err = a.firstBootScript(ctx, v)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "#!/bin/sh\n"))
	assert.Contains(t, got, "echo vpc")

	hook.script = ""
	got, err = a.firstBootScript(ctx, v)
	require.NoError(t, err)
	assert.Empty(t, got, "rien à faire")

	hook.err = errors.New("base indisponible")
	_, err = a.firstBootScript(ctx, v)
	assert.Error(t, err, "rejoué par Temporal")
}
