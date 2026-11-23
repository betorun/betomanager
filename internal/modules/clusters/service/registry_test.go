package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

type memRegistry struct {
	mu sync.Mutex
	cs map[string]*domain.Cluster
	hs map[string]*domain.Health
}

func newMemRegistry() *memRegistry {
	return &memRegistry{cs: map[string]*domain.Cluster{}, hs: map[string]*domain.Health{}}
}

func (m *memRegistry) List(context.Context) ([]domain.Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Cluster{}
	for _, c := range m.cs {
		out = append(out, *c)
	}
	return out, nil
}
func (m *memRegistry) Get(_ context.Context, id string) (*domain.Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cs[id]
	if !ok {
		return nil, domain.ErrClusterNotFound
	}
	cp := *c
	return &cp, nil
}
func (m *memRegistry) Declare(_ context.Context, c *domain.Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cs[c.ClusterID]; ok {
		return domain.ErrClusterExists
	}
	cp := *c
	m.cs[c.ClusterID] = &cp
	return nil
}
func (m *memRegistry) Update(_ context.Context, id string, f map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cs[id]
	if !ok {
		return domain.ErrClusterNotFound
	}
	for k, v := range f {
		switch k {
		case "state":
			c.State = v.(string)
		case "name":
			c.Name = v.(string)
		case "kubeconfig_ciphertext":
			c.KubeconfigCiphertext = v.([]byte)
		case "api_server":
			c.APIServer = v.(string)
		case "kubeconfig_set_at":
			t := v.(time.Time)
			c.KubeconfigSetAt = &t
		}
	}
	return nil
}
func (m *memRegistry) SetDefault(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cs {
		c.IsDefault = c.ClusterID == id
	}
	return nil
}
func (m *memRegistry) EnsureDefault(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cs[id]; !ok {
		m.cs[id] = &domain.Cluster{ClusterID: id, Name: id, Provider: domain.ProviderOther, Distribution: domain.DistributionK3s, State: domain.StateActive, IsDefault: true}
	}
	return nil
}
func (m *memRegistry) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cs, id)
	return nil
}
func (m *memRegistry) Healths(context.Context) (map[string]*domain.Health, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*domain.Health{}
	for k, v := range m.hs {
		cp := *v
		out[k] = &cp
	}
	return out, nil
}
func (m *memRegistry) SaveHealth(_ context.Context, h *domain.Health) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *h
	m.hs[h.ClusterID] = &cp
	return nil
}

// xorSealer : chiffrement factice qui vérifie l'AAD.
type xorSealer struct{}

func (xorSealer) Seal(p []byte, aad string) ([]byte, error) {
	return append([]byte(aad+"|"), p...), nil
}
func (xorSealer) Open(c []byte, aad string) ([]byte, error) {
	prefix := aad + "|"
	if len(c) < len(prefix) || string(c[:len(prefix)]) != prefix {
		return nil, errors.New("aad mismatch")
	}
	return c[len(prefix):], nil
}

type fakeReader struct {
	snap *domain.Snapshot
	err  error
}

func (f fakeReader) Snapshot(context.Context) (*domain.Snapshot, error) { return f.snap, f.err }
func (f fakeReader) Nodes(context.Context) ([]domain.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snap.Nodes, nil
}

func healthy() *domain.Snapshot {
	return &domain.Snapshot{Version: "v1.31.2+k3s1", Nodes: []domain.Node{{Name: "n1", Ready: true, CPUAllocMilli: 4000, MemAllocBytes: 8 << 30}}}
}

func newRegistry(store *memRegistry, dial map[string]fakeReader) *Registry {
	return &Registry{Store: store, Sealer: xorSealer{}, DefaultID: "terrain", Default: fakeReader{snap: healthy()},
		Dial: func(raw []byte) (Reader, string, error) {
			r, ok := dial[string(raw)]
			if !ok {
				return nil, "", errors.New("bad kubeconfig")
			}
			return r, "https://" + string(raw) + ":6443", nil
		}}
}

func TestRegistry_DeclareAndRules(t *testing.T) {
	ctx := context.Background()
	store := newMemRegistry()
	r := newRegistry(store, nil)
	require.NoError(t, r.EnsureDefault(ctx))

	v, err := r.Declare(ctx, DeclareInput{ClusterID: "prox-gra-1", Provider: domain.ProviderProxmox, Distribution: domain.DistributionK8s, Region: "eu-west-gra"}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "prox-gra-1", v.Name, "nom par défaut : l'identifiant")
	assert.False(t, v.HasKubeconfig)
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "prox-gra-1", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s}, "")
	assert.ErrorIs(t, err, domain.ErrClusterExists)
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "Bad_ID", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidClusterID)

	disabled := domain.StateDisabled
	_, err = r.Update(ctx, "terrain", UpdateInput{State: &disabled})
	assert.ErrorIs(t, err, domain.ErrDefaultCluster, "le défaut ne se met pas hors service")
	assert.ErrorIs(t, r.Delete(ctx, "terrain"), domain.ErrDefaultCluster)

	_, err = r.Update(ctx, "prox-gra-1", UpdateInput{State: &disabled})
	require.NoError(t, err)
	_, err = r.SetDefault(ctx, "prox-gra-1")
	assert.ErrorIs(t, err, domain.ErrDisabledDefault)

	list, err := r.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, c := range list {
		if c.ClusterID == "terrain" {
			assert.True(t, c.FromConfig, "joint par la configuration")
		}
	}
}

// L'accès n'est gardé que si le cluster répond ; il est chiffré, lié au cluster.
func TestRegistry_SetKubeconfig(t *testing.T) {
	ctx := context.Background()
	store := newMemRegistry()
	r := newRegistry(store, map[string]fakeReader{"good": {snap: healthy()}, "dead": {err: errors.New("dial tcp: timeout")}})
	_, err := r.Declare(ctx, DeclareInput{ClusterID: "ovh-rbx-1", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s}, "")
	require.NoError(t, err)

	_, err = r.SetKubeconfig(ctx, "ovh-rbx-1", []byte("dead"), "user:staff")
	assert.ErrorIs(t, err, domain.ErrClusterUnreachable)
	assert.Empty(t, store.cs["ovh-rbx-1"].KubeconfigCiphertext, "accès refusé : rien n'est gardé")

	v, err := r.SetKubeconfig(ctx, "ovh-rbx-1", []byte("good"), "user:staff")
	require.NoError(t, err)
	assert.True(t, v.HasKubeconfig)
	assert.Equal(t, "https://good:6443", v.APIServer)
	require.NotNil(t, v.Health)
	assert.Equal(t, domain.HealthDegraded, v.Health.State, "watcher pas encore venu")
	assert.NotEqual(t, "good", string(store.cs["ovh-rbx-1"].KubeconfigCiphertext))

	// Le chiffré d'un cluster ne sert pas à un autre (AAD).
	store.cs["ovh-rbx-1"].KubeconfigCiphertext, _ = xorSealer{}.Seal([]byte("good"), aad("autre"))
	at := time.Now()
	store.cs["ovh-rbx-1"].KubeconfigSetAt = &at
	_, err = r.Nodes(ctx, "ovh-rbx-1")
	assert.Error(t, err)
}

func TestRegistry_CheckAll(t *testing.T) {
	ctx := context.Background()
	store := newMemRegistry()
	r := newRegistry(store, nil)
	require.NoError(t, r.EnsureDefault(ctx))
	now := time.Now().UTC()
	store.cs["terrain"].LastSeenAt = &now
	_, err := r.Declare(ctx, DeclareInput{ClusterID: "sans-acces", Provider: domain.ProviderOther, Distribution: domain.DistributionK8s}, "")
	require.NoError(t, err)
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "eteint", Provider: domain.ProviderOther, Distribution: domain.DistributionK8s}, "")
	require.NoError(t, err)
	off := domain.StateDisabled
	_, err = r.Update(ctx, "eteint", UpdateInput{State: &off})
	require.NoError(t, err)

	require.NoError(t, r.CheckAll(ctx))
	assert.Equal(t, domain.HealthOK, store.hs["terrain"].State)
	assert.Equal(t, domain.HealthUnknown, store.hs["sans-acces"].State)
	assert.Contains(t, []string(store.hs["sans-acces"].Reasons), domain.ReasonNoAccess)
	assert.NotContains(t, store.hs, "eteint", "hors service : plus sondé")
}
