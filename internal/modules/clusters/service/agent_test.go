package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

type fakeInstaller struct {
	adminDenied bool
	agentErr    error
	agents      []domain.AgentSpec
	platform    int
	egressRBAC  int
	egressImage string
}

func (f *fakeInstaller) Platform(context.Context) ([]byte, []byte, error) {
	if f.adminDenied {
		return nil, nil, domain.ErrAdminRequired
	}
	f.platform++
	return []byte("tok"), []byte("ca"), nil
}
func (f *fakeInstaller) AgentRBAC(context.Context) error { return nil }
func (f *fakeInstaller) EgressAgentRBAC(context.Context) error {
	f.egressRBAC++
	return nil
}
func (f *fakeInstaller) EgressAgent(_ context.Context, image string) error {
	f.egressImage = image
	return nil
}
func (f *fakeInstaller) EgressAgentImage(context.Context) (string, error) { return f.egressImage, nil }
func (f *fakeInstaller) Agent(_ context.Context, s domain.AgentSpec) error {
	if f.agentErr != nil {
		return f.agentErr
	}
	f.agents = append(f.agents, s)
	return nil
}
func (f *fakeInstaller) State(context.Context) (*domain.AgentState, error) {
	st := &domain.AgentState{Installed: true, Desired: 1, Ready: 1, ManagedByPlatform: true}
	if n := len(f.agents); n > 0 {
		st.Image = f.agents[n-1].Image
	}
	return st, nil
}

type fakeKeys struct {
	issued, revoked []string
}

func (k *fakeKeys) Issue(_ context.Context, _, clusterID string) (string, []byte, error) {
	id := clusterID + "-k" + string(rune('1'+len(k.issued)))
	k.issued = append(k.issued, id)
	return id, []byte(`{"key_id":"` + id + `"}`), nil
}
func (k *fakeKeys) Revoke(_ context.Context, id string) error {
	k.revoked = append(k.revoked, id)
	return nil
}

func newAgents(t *testing.T) (*Agents, *memRegistry, *fakeInstaller, *fakeInstaller, *fakeKeys) {
	t.Helper()
	ctx := context.Background()
	store := newMemRegistry()
	// Le kubeconfig restreint (« restricted ») répond ; celui d'admin n'est jamais composé.
	reg := newRegistry(store, map[string]fakeReader{"restricted": {snap: healthy()}})
	require.NoError(t, reg.EnsureDefault(ctx))
	_, err := reg.Declare(ctx, DeclareInput{ClusterID: "gra-1", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s}, "")
	require.NoError(t, err)
	admin, restricted, keys := &fakeInstaller{}, &fakeInstaller{}, &fakeKeys{}
	ag := &Agents{Registry: reg, Keys: keys, Image: "thekrauss/watcher:abc123", APIURL: "https://api.example/kmanager/v1/clusters",
		Prepare: func(raw []byte) (AgentInstaller, string, error) {
			if string(raw) != "admin" {
				return nil, "", errors.New("bad kubeconfig")
			}
			return admin, "https://gra-1:6443", nil
		},
		For: func(context.Context, string) (AgentInstaller, error) { return restricted, nil },
		RestrictedKubeconfig: func(id, server string, token, ca []byte) ([]byte, error) {
			return []byte("restricted"), nil
		},
	}
	return ag, store, admin, restricted, keys
}

// Préparation : compte plateforme et agent posés avec l'accès admin ; seul
// l'accès restreint est enregistré ; la clé est suivie.
func TestAgents_Bootstrap(t *testing.T) {
	ctx := context.Background()
	ag, store, admin, _, keys := newAgents(t)

	st, err := ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "user:staff")
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, "thekrauss/watcher:abc123", st.Image)
	require.Len(t, admin.agents, 1)
	spec := admin.agents[0]
	assert.Equal(t, "gra-1", spec.ClusterID)
	assert.Equal(t, "https://api.example/kmanager/v1/clusters", spec.APIURL)
	assert.JSONEq(t, `{"key_id":"gra-1-k1"}`, string(spec.KeyFile))

	c, _ := store.Get(ctx, "gra-1")
	opened, err := xorSealer{}.Open(c.KubeconfigCiphertext, aad("gra-1"))
	require.NoError(t, err)
	assert.Equal(t, "restricted", string(opened), "jamais le kubeconfig admin")
	assert.Equal(t, "gra-1-k1", c.AgentKeyID)
	assert.NotNil(t, c.AgentInstalledAt)

	// Refaite : nouvelle clé, l'ancienne révoquée.
	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin"), Image: "thekrauss/watcher:def456"}, "user:staff")
	require.NoError(t, err)
	c, _ = store.Get(ctx, "gra-1")
	assert.Equal(t, "gra-1-k2", c.AgentKeyID)
	assert.Equal(t, []string{"gra-1-k1"}, keys.revoked)
	assert.Equal(t, "thekrauss/watcher:def456", c.AgentImage)
}

func TestAgents_BootstrapRefusals(t *testing.T) {
	ctx := context.Background()
	ag, store, admin, _, keys := newAgents(t)

	admin.adminDenied = true
	_, err := ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	assert.ErrorIs(t, err, domain.ErrAdminRequired)
	assert.Empty(t, keys.issued, "aucune clé émise sans les droits")
	admin.adminDenied = false

	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin"), Image: "Bad Image"}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidAgentImage)

	// Agent refusé par le cluster : la clé émise est révoquée.
	admin.agentErr = errors.New("quota")
	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	assert.Error(t, err)
	assert.Equal(t, keys.issued, keys.revoked)
	c, _ := store.Get(ctx, "gra-1")
	assert.Empty(t, c.KubeconfigCiphertext, "rien d'enregistré")
	admin.agentErr = nil

	ag.Image = ""
	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	assert.ErrorIs(t, err, domain.ErrAgentImageMissing)
	ag.Image = "thekrauss/watcher:abc"

	ag.APIURL = ""
	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	assert.ErrorIs(t, err, domain.ErrAgentAPIURLMissing)
}

// Mise à jour par le compte restreint : image seule (clé gardée) ou rotation.
func TestAgents_Update(t *testing.T) {
	ctx := context.Background()
	ag, store, _, restricted, keys := newAgents(t)
	_, err := ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	require.NoError(t, err)

	st, err := ag.Update(ctx, "gra-1", AgentInput{Image: "thekrauss/watcher:new"}, "")
	require.NoError(t, err)
	assert.Equal(t, "thekrauss/watcher:new", st.Image)
	require.Len(t, restricted.agents, 1)
	assert.Empty(t, restricted.agents[0].KeyFile, "clé gardée")
	c, _ := store.Get(ctx, "gra-1")
	assert.Equal(t, "gra-1-k1", c.AgentKeyID)
	assert.Empty(t, keys.revoked)

	_, err = ag.Update(ctx, "gra-1", AgentInput{RotateKey: true}, "")
	require.NoError(t, err)
	assert.NotEmpty(t, restricted.agents[1].KeyFile)
	assert.Equal(t, "thekrauss/watcher:new", restricted.agents[1].Image, "image en place gardée")
	c, _ = store.Get(ctx, "gra-1")
	assert.Equal(t, "gra-1-k2", c.AgentKeyID)
	assert.Equal(t, []string{"gra-1-k1"}, keys.revoked)
}

// Agent réseau : rôle posé à la préparation, DaemonSet seulement avec une
// image configurée ; suivi à chaque mise à jour.
func TestAgents_EgressAgent(t *testing.T) {
	ctx := context.Background()
	ag, _, admin, restricted, _ := newAgents(t)
	_, err := ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	require.NoError(t, err)
	assert.Equal(t, 1, admin.egressRBAC)
	assert.Empty(t, admin.egressImage, "sans image : pas de DaemonSet")

	ag.EgressImage = "thekrauss/egress-agent:abc"
	_, err = ag.Bootstrap(ctx, "gra-1", BootstrapInput{Kubeconfig: []byte("admin")}, "")
	require.NoError(t, err)
	assert.Equal(t, "thekrauss/egress-agent:abc", admin.egressImage)

	ag.EgressImage = "thekrauss/egress-agent:def"
	st, err := ag.Update(ctx, "gra-1", AgentInput{}, "")
	require.NoError(t, err)
	assert.Equal(t, "thekrauss/egress-agent:def", restricted.egressImage, "mise à jour par le compte restreint")
	assert.Equal(t, 0, restricted.egressRBAC, "jamais de RBAC hors préparation")
	_ = st

	ag.EgressImage = "Bad Image"
	_, err = ag.Update(ctx, "gra-1", AgentInput{}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidAgentImage)
}
