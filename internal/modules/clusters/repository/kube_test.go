package repository

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const baseKubeconfig = `apiVersion: v1
kind: Config
current-context: c
clusters:
- name: k
  cluster:
    server: SERVER
    CLUSTERCA
contexts:
- name: c
  context: {cluster: k, user: u}
users:
- name: u
  user:
    USER
`

func kubeconfig(server, ca, user string) []byte {
	return []byte(strings.NewReplacer("SERVER", server, "CLUSTERCA", ca, "USER", user).Replace(baseKubeconfig))
}

func TestParseKubeconfig(t *testing.T) {
	okCA := "certificate-authority-data: " + "dGVzdA=="
	rc, server, err := ParseKubeconfig(kubeconfig("https://10.0.0.1:6443/", okCA, "token: abc"))
	require.NoError(t, err)
	assert.Equal(t, "https://10.0.0.1:6443", server)
	assert.Equal(t, "abc", rc.BearerToken)
	assert.Equal(t, requestTimeout, rc.Timeout)

	cases := map[string]struct {
		raw  []byte
		want error
	}{
		"commande exec":      {kubeconfig("https://k:6443", okCA, "exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/sh, args: [-c, id]}"), ErrKubeconfigExec},
		"auth-provider":      {kubeconfig("https://k:6443", okCA, "auth-provider: {name: gcp}"), ErrKubeconfigExec},
		"jeton fichier":      {kubeconfig("https://k:6443", okCA, "tokenFile: /etc/passwd"), ErrKubeconfigFiles},
		"certificat fichier": {kubeconfig("https://k:6443", okCA, "client-certificate: /root/c.pem"), ErrKubeconfigFiles},
		"AC fichier":         {kubeconfig("https://k:6443", "certificate-authority: /root/ca.pem", "token: abc"), ErrKubeconfigFiles},
		"http":               {kubeconfig("http://k:6443", okCA, "token: abc"), ErrKubeconfigHTTPS},
		"sans accès":         {kubeconfig("https://k:6443", okCA, "{}"), ErrKubeconfigNoCredentials},
		"vide":               {nil, ErrKubeconfigInvalid},
		"pas du yaml":        {[]byte("::::"), ErrKubeconfigNoContext},
		"yaml cassé":         {[]byte("clusters: [unclosed"), ErrKubeconfigInvalid},
		"trop gros":          {[]byte(strings.Repeat("a", 70<<10)), ErrKubeconfigInvalid},
	}
	for name, c := range cases {
		_, _, err := ParseKubeconfig(c.raw)
		assert.ErrorIs(t, err, c.want, name)
	}
}

// Un utilisateur hors du contexte courant ne rouvre pas la porte aux commandes.
func TestParseKubeconfig_ChecksEveryUser(t *testing.T) {
	raw := string(kubeconfig("https://k:6443", "certificate-authority-data: dGVzdA==", "token: abc")) +
		"- name: other\n  user:\n    exec: {apiVersion: client.authentication.k8s.io/v1, command: curl}\n"
	_, _, err := ParseKubeconfig([]byte(raw))
	assert.ErrorIs(t, err, ErrKubeconfigExec)
}

// Relevé réel : CLUSTER_LIVE_KUBECONFIG=<fichier> go test -run Live ./internal/modules/clusters/repository/
func TestSnapshot_Live(t *testing.T) {
	path := os.Getenv("CLUSTER_LIVE_KUBECONFIG")
	if path == "" {
		t.Skip("CLUSTER_LIVE_KUBECONFIG not set")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	rc, server, err := ParseKubeconfig(raw)
	require.NoError(t, err)
	k, err := NewKube(rc)
	require.NoError(t, err)
	snap, err := k.Snapshot(context.Background())
	require.NoError(t, err)
	t.Logf("server=%s version=%s nodes=%d running=%d failing=%d", server, snap.Version, len(snap.Nodes), snap.PodsRunning, snap.PodsFailing)
	for _, n := range snap.Nodes {
		t.Logf("node %s ready=%v roles=%v cpu=%d/%dm mem=%d/%dMi pods=%d", n.Name, n.Ready, n.Roles, n.CPUReqMilli, n.CPUAllocMilli, n.MemReqBytes>>20, n.MemAllocBytes>>20, n.Pods)
	}
	assert.NotEmpty(t, snap.Nodes)
}
