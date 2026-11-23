package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/egressagent"
	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
	"github.com/thekrauss/kubemanager/internal/modules/egress/repository"
)

type memStore struct {
	mu      sync.Mutex
	addrs   []*domain.Address
	places  map[uuid.UUID]*repository.ProjectPlace
	deleted map[uuid.UUID]bool
}

func (m *memStore) List(_ context.Context, c string) ([]domain.AdminAddress, error) {
	var out []domain.AdminAddress
	for _, a := range m.addrs {
		if a.ClusterID == c {
			out = append(out, domain.AdminAddress{Address: *a, Assigned: a.ProjectID != nil})
		}
	}
	return out, nil
}
func (m *memStore) Declare(_ context.Context, a *domain.Address) error {
	for _, x := range m.addrs {
		if x.Address == a.Address {
			return domain.ErrAddressExists
		}
	}
	m.addrs = append(m.addrs, a)
	return nil
}
func (m *memStore) Delete(_ context.Context, c string, id uuid.UUID) error {
	for i, a := range m.addrs {
		if a.ID == id && a.ClusterID == c {
			if a.ProjectID != nil {
				return domain.ErrAddressInUse
			}
			m.addrs = append(m.addrs[:i], m.addrs[i+1:]...)
			return nil
		}
	}
	return domain.ErrAddressNotFound
}
func (m *memStore) ForProject(_ context.Context, p uuid.UUID) (*domain.Address, error) {
	for _, a := range m.addrs {
		if a.ProjectID != nil && *a.ProjectID == p {
			c := *a
			return &c, nil
		}
	}
	return nil, nil
}
func (m *memStore) FreeCount(_ context.Context, c string) (int, error) {
	n := 0
	for _, a := range m.addrs {
		if a.ClusterID == c && a.ProjectID == nil {
			n++
		}
	}
	return n, nil
}
func (m *memStore) Assign(_ context.Context, c string, p uuid.UUID, by string, now time.Time, only []string) (*domain.Address, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.Slice(m.addrs, func(i, j int) bool { return m.addrs[i].Address < m.addrs[j].Address })
	allowed := func(a string) bool {
		if only == nil {
			return true
		}
		for _, o := range only {
			if o == a {
				return true
			}
		}
		return false
	}
	for _, a := range m.addrs {
		if a.ClusterID == c && a.ProjectID == nil && allowed(a.Address) {
			a.ProjectID, a.AssignedAt, a.AssignedBy = &p, &now, by
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *memStore) Release(_ context.Context, id uuid.UUID, _ time.Time) error {
	for _, a := range m.addrs {
		if a.ID == id {
			a.ProjectID, a.AssignedAt, a.AssignedBy = nil, nil, ""
		}
	}
	return nil
}
func (m *memStore) Place(_ context.Context, p uuid.UUID, def string) (*repository.ProjectPlace, error) {
	pl, ok := m.places[p]
	if !ok || m.deleted[p] {
		return nil, nil
	}
	c := *pl
	if c.ClusterID == "" {
		c.ClusterID = def
	}
	return &c, nil
}
func (m *memStore) Clusters(context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, a := range m.addrs {
		if !seen[a.ClusterID] {
			seen[a.ClusterID] = true
			out = append(out, a.ClusterID)
		}
	}
	sort.Strings(out)
	return out, nil
}
func (m *memStore) Orphans(context.Context) ([]repository.Orphan, error) {
	var out []repository.Orphan
	for _, a := range m.addrs {
		if a.ProjectID != nil && m.deleted[*a.ProjectID] {
			out = append(out, repository.Orphan{Address: *a, Namespace: m.places[*a.ProjectID].Namespace})
		}
	}
	return out, nil
}

type fakeGateway struct {
	applied map[string]domain.Binding
	removed []string
	fail    error
}

func (g *fakeGateway) Apply(_ context.Context, b domain.Binding) error {
	if g.fail != nil {
		return g.fail
	}
	g.applied[b.Namespace] = b
	return nil
}
func (g *fakeGateway) Remove(_ context.Context, _, ns string) error {
	g.removed = append(g.removed, ns)
	delete(g.applied, ns)
	return nil
}

func setup(t *testing.T) (*Service, *memStore, *fakeGateway, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	onGra, onGra2, onPlain := uuid.New(), uuid.New(), uuid.New()
	store := &memStore{deleted: map[uuid.UUID]bool{}, places: map[uuid.UUID]*repository.ProjectPlace{
		onGra:   {Namespace: "km-a", ClusterID: "gra-1", Gateway: domain.GatewayCilium},
		onGra2:  {Namespace: "km-b", ClusterID: "gra-1", Gateway: domain.GatewayCilium},
		onPlain: {Namespace: "km-c", ClusterID: "", Gateway: domain.GatewayNone},
	}}
	gw := &fakeGateway{applied: map[string]domain.Binding{}}
	s := &Service{Store: store, Gateways: map[string]Gateway{domain.GatewayCilium: gw}, DefaultCluster: "main"}
	return s, store, gw, onGra, onGra2, onPlain
}

func TestEgress_RequestAndRelease(t *testing.T) {
	ctx := context.Background()
	s, _, gw, p1, p2, plain := setup(t)

	_, err := s.Declare(ctx, "gra-1", DeclareInput{Address: "10.0.0.5", GatewayNode: "n1"}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidAddress, "adresse privée refusée")
	_, err = s.Declare(ctx, "gra-1", DeclareInput{Address: "203.0.113.7", GatewayNode: "Bad Node"}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidNode)
	_, err = s.Declare(ctx, "gra-1", DeclareInput{Address: " 203.0.113.7 ", GatewayNode: "gw-1"}, "user:staff")
	require.NoError(t, err)
	_, err = s.Declare(ctx, "gra-1", DeclareInput{Address: "203.0.113.7", GatewayNode: "gw-1"}, "")
	assert.ErrorIs(t, err, domain.ErrAddressExists)

	st, err := s.Get(ctx, p1)
	require.NoError(t, err)
	assert.Equal(t, domain.ProjectEgress{Offered: true, Available: true}, *st)
	st, err = s.Get(ctx, plain)
	require.NoError(t, err)
	assert.False(t, st.Offered, "cluster sans passerelle")
	_, err = s.Request(ctx, plain, "")
	assert.ErrorIs(t, err, domain.ErrNoGateway)

	got, err := s.Request(ctx, p1, "user:a")
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", got.Address)
	assert.Equal(t, domain.Binding{ClusterID: "gra-1", Namespace: "km-a", Address: "203.0.113.7", GatewayNode: "gw-1"}, gw.applied["km-a"])
	_, err = s.Request(ctx, p1, "")
	assert.ErrorIs(t, err, domain.ErrAlreadyHasAddress)
	_, err = s.Request(ctx, p2, "")
	assert.ErrorIs(t, err, domain.ErrNoFreeAddress, "une seule IP dans la réserve")
	st, _ = s.Get(ctx, p2)
	assert.True(t, st.Offered)
	assert.False(t, st.Available)

	list, _ := s.List(ctx, "gra-1")
	require.Len(t, list, 1)
	assert.ErrorIs(t, s.Remove(ctx, "gra-1", list[0].ID), domain.ErrAddressInUse)

	require.NoError(t, s.Release(ctx, p1))
	assert.Equal(t, []string{"km-a"}, gw.removed)
	assert.ErrorIs(t, s.Release(ctx, p1), domain.ErrNoAddress)
	got, err = s.Request(ctx, p2, "")
	require.NoError(t, err, "IP rendue : réattribuable")
	assert.Equal(t, "203.0.113.7", got.Address)
}

// Règle refusée par le cluster : l'IP revient dans la réserve.
func TestEgress_ApplyFailureReleases(t *testing.T) {
	ctx := context.Background()
	s, store, gw, p1, _, _ := setup(t)
	_, err := s.Declare(ctx, "gra-1", DeclareInput{Address: "203.0.113.8", GatewayNode: "gw-1"}, "")
	require.NoError(t, err)
	gw.fail = repository.ErrGatewayMissing
	_, err = s.Request(ctx, p1, "")
	assert.ErrorIs(t, err, repository.ErrGatewayMissing)
	n, _ := store.FreeCount(ctx, "gra-1")
	assert.Equal(t, 1, n)
}

// Projet supprimé : son IP est rendue et sa règle retirée par la passe du worker.
func TestEgress_SweepOrphans(t *testing.T) {
	ctx := context.Background()
	s, store, gw, p1, _, _ := setup(t)
	_, err := s.Declare(ctx, "gra-1", DeclareInput{Address: "203.0.113.9", GatewayNode: "gw-1"}, "")
	require.NoError(t, err)
	_, err = s.Request(ctx, p1, "")
	require.NoError(t, err)
	store.deleted[p1] = true
	s.Sweep(ctx)
	assert.Contains(t, gw.removed, "km-a")
	n, _ := store.FreeCount(ctx, "gra-1")
	assert.Equal(t, 1, n)
	_, err = s.Get(ctx, p1)
	assert.True(t, errors.Is(err, domain.ErrProjectNotFound))
}

type fakeNodes struct {
	published map[string]egressagent.Desired
	mounted   map[string]map[string]bool
	agent     bool
}

func (f *fakeNodes) Publish(_ context.Context, c string, d egressagent.Desired) error {
	if f.published == nil {
		f.published = map[string]egressagent.Desired{}
	}
	f.published[c] = d
	return nil
}
func (f *fakeNodes) Mounted(context.Context, string) (map[string]map[string]bool, bool, error) {
	return f.mounted, f.agent, nil
}

// Lot 2 : la réserve est publiée vers le cluster ; avec un agent réseau, une
// IP n'est donnée que si son nœud la porte vraiment.
func TestEgress_AgentMountedOnly(t *testing.T) {
	ctx := context.Background()
	s, _, _, p1, p2, _ := setup(t)
	nodes := &fakeNodes{}
	s.Nodes = nodes
	for _, ip := range []string{"203.0.113.7", "203.0.113.8"} {
		_, err := s.Declare(ctx, "gra-1", DeclareInput{Address: ip, GatewayNode: "gw-1"}, "")
		require.NoError(t, err)
	}
	assert.ElementsMatch(t, []string{"203.0.113.7", "203.0.113.8"}, nodes.published["gra-1"]["gw-1"], "toute la réserve publiée")

	// Pas d'agent : montage à la main, toute IP libre convient ; état inconnu.
	list, _ := s.List(ctx, "gra-1")
	assert.Nil(t, list[0].Mounted)
	st, _ := s.Get(ctx, p1)
	assert.True(t, st.Available)

	// Agent présent, rien de monté : rien à donner.
	nodes.agent, nodes.mounted = true, map[string]map[string]bool{}
	st, _ = s.Get(ctx, p1)
	assert.False(t, st.Available)
	_, err := s.Request(ctx, p1, "")
	assert.ErrorIs(t, err, domain.ErrNoFreeAddress)

	// .8 montée seulement : c'est elle qui est donnée.
	nodes.mounted = map[string]map[string]bool{"gw-1": {"203.0.113.8": true}}
	got, err := s.Request(ctx, p1, "")
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.8", got.Address)
	_, err = s.Request(ctx, p2, "")
	assert.ErrorIs(t, err, domain.ErrNoFreeAddress, ".7 pas encore montée")
	list, _ = s.List(ctx, "gra-1")
	for _, a := range list {
		require.NotNil(t, a.Mounted)
		assert.Equal(t, a.Address.Address == "203.0.113.8", *a.Mounted)
	}

	// Retrait : la consigne suit.
	for _, a := range list {
		if !a.Assigned {
			require.NoError(t, s.Remove(ctx, "gra-1", a.ID))
		}
	}
	assert.Equal(t, []string{"203.0.113.8"}, nodes.published["gra-1"]["gw-1"])
}
