package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	k8s "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

type placements struct {
	byProject map[uuid.UUID]string
	byNS      map[string]string
	calls     int
}

func (p *placements) ProjectCluster(_ context.Context, id uuid.UUID) (string, bool, error) {
	p.calls++
	c, ok := p.byProject[id]
	return c, ok, nil
}
func (p *placements) NamespaceCluster(_ context.Context, ns string) (string, bool, error) {
	p.calls++
	c, ok := p.byNS[ns]
	return c, ok, nil
}

func TestPool(t *testing.T) {
	ctx := context.Background()
	store := newMemRegistry()
	reg := newRegistry(store, nil)
	require.NoError(t, reg.EnsureDefault(ctx))
	_, err := reg.Declare(ctx, DeclareInput{ClusterID: "prox-1", Provider: domain.ProviderProxmox, Distribution: domain.DistributionK8s}, "")
	require.NoError(t, err)
	at := time.Now()
	store.cs["prox-1"].KubeconfigCiphertext, _ = xorSealer{}.Seal([]byte("kc"), aad("prox-1"))
	store.cs["prox-1"].KubeconfigSetAt = &at

	def := &k8s.ProviderK8s{}
	onDefault, onProx := uuid.New(), uuid.New()
	pl := &placements{byProject: map[uuid.UUID]string{onDefault: "", onProx: "prox-1"}, byNS: map[string]string{"km-x": "prox-1"}}
	dialed := 0
	pool := &Pool{Registry: reg, Projects: pl, Default: def, RestConfig: func(raw []byte) (*rest.Config, error) {
		dialed++
		assert.Equal(t, "kc", string(raw))
		return &rest.Config{Host: "https://prox:6443"}, nil
	}}

	got, err := pool.ForProject(ctx, onDefault)
	require.NoError(t, err)
	assert.Same(t, def, got, "sans cluster : celui de la configuration")

	got, err = pool.ForProject(ctx, onProx)
	require.NoError(t, err)
	assert.Equal(t, "prox-1", got.ClusterID)
	again, err := pool.ForNamespace(ctx, "km-x")
	require.NoError(t, err)
	assert.Same(t, got, again, "un client par cluster, réutilisé")
	assert.Equal(t, 1, dialed)

	calls := pl.calls
	_, _ = pool.ForProject(ctx, onProx)
	assert.Equal(t, calls, pl.calls, "rattachement gardé en cache")

	_, err = pool.ForProject(ctx, uuid.New())
	assert.ErrorIs(t, err, ErrUnknownProject)

	// Nouvel accès déposé : nouveau client.
	later := at.Add(time.Minute)
	store.cs["prox-1"].KubeconfigSetAt = &later
	_, err = pool.ForCluster(ctx, "prox-1")
	require.NoError(t, err)
	assert.Equal(t, 2, dialed)

	_, err = (&Pool{Registry: reg, Projects: pl}).ForCluster(ctx, "")
	assert.ErrorIs(t, err, ErrNoDefaultCluster)
}
