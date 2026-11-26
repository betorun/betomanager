package service

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/backend/memory"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// fakeStore : même règles d'attribution que repository.Store, en mémoire.
type fakeStore struct {
	mu    sync.Mutex
	nets  map[uuid.UUID]*domain.Network
	atts  map[uuid.UUID]*domain.Attachment
	enrol map[string]*domain.Enrollment
}

func newFakeStore() *fakeStore {
	return &fakeStore{nets: map[uuid.UUID]*domain.Network{}, atts: map[uuid.UUID]*domain.Attachment{}, enrol: map[string]*domain.Enrollment{}}
}

func (f *fakeStore) Network(_ context.Context, p uuid.UUID) (*domain.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.nets[p]; ok {
		c := *n
		return &c, nil
	}
	return nil, nil
}

func (f *fakeStore) Attachment(_ context.Context, v uuid.UUID) (*domain.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.atts[v]; ok {
		c := *a
		return &c, nil
	}
	return nil, nil
}

func (f *fakeStore) Attachments(_ context.Context, p uuid.UUID) ([]domain.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Attachment
	for _, a := range f.atts {
		if a.ProjectID == p {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeStore) Attach(_ context.Context, p uuid.UUID, cluster string, a *domain.Attachment) (*domain.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.atts[a.VPSID]; ok {
		return nil, domain.ErrAlreadyAttached
	}
	n, ok := f.nets[p]
	if !ok {
		var used []netip.Prefix
		for _, x := range f.nets {
			if x.ClusterID == cluster {
				used = append(used, netip.MustParsePrefix(x.CIDR))
			}
		}
		r, err := domain.NextRange(used)
		if err != nil {
			return nil, err
		}
		n = &domain.Network{ProjectID: p, ClusterID: cluster, CIDR: r.String()}
		f.nets[p] = n
	}
	var taken []netip.Addr
	for _, x := range f.atts {
		if x.ProjectID == p {
			taken = append(taken, netip.MustParseAddr(x.Address))
		}
	}
	addr, err := domain.NextAddress(netip.MustParsePrefix(n.CIDR), taken)
	if err != nil {
		return nil, err
	}
	a.ProjectID, a.Address = p, addr.String()
	c := *a
	f.atts[a.VPSID] = &c
	nc := *n
	return &nc, nil
}

func (f *fakeStore) SetState(_ context.Context, v uuid.UUID, state, key string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.atts[v]
	a.State = state
	if state == domain.StateAttached {
		a.AttachedAt, a.LastContactAt = &at, &at
	}
	if key != "" {
		a.PublicKey = key
	}
	return nil
}

func (f *fakeStore) Detach(_ context.Context, v, p uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.atts, v)
	for k, e := range f.enrol {
		if e.VPSID == v {
			delete(f.enrol, k)
		}
	}
	var left int64
	for _, a := range f.atts {
		if a.ProjectID == p {
			left++
		}
	}
	return left, nil
}

func (f *fakeStore) NewEnrollment(_ context.Context, e *domain.Enrollment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, x := range f.enrol {
		if x.VPSID == e.VPSID && x.UsedAt == nil {
			delete(f.enrol, k)
		}
	}
	f.enrol[string(e.TokenHash)] = e
	return nil
}

func (f *fakeStore) ConsumeEnrollment(_ context.Context, hash []byte, now time.Time) (*domain.Enrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.enrol[string(hash)]
	if !ok || e.UsedAt != nil || !e.ExpiresAt.After(now) {
		return nil, nil
	}
	e.UsedAt = &now
	return e, nil
}

type fakeMachines map[uuid.UUID]*computeDomain.VPS

func (m fakeMachines) VPS(_ context.Context, id uuid.UUID) (*computeDomain.VPS, error) {
	if v, ok := m[id]; ok {
		return v, nil
	}
	return nil, computeDomain.ErrVPSNotFound
}

type fakeClusters struct{ c Cluster }

func (f fakeClusters) ProjectCluster(context.Context, uuid.UUID) (Cluster, error) { return f.c, nil }

type fixture struct {
	svc     *Service
	backend *memory.Backend
	org     uuid.UUID
	project uuid.UUID
	vms     fakeMachines
	now     time.Time
}

func newFixture(t *testing.T, enrollment bool) *fixture {
	t.Helper()
	f := &fixture{backend: &memory.Backend{Enrollment: enrollment}, org: uuid.New(), project: uuid.New(), vms: fakeMachines{},
		now: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}
	f.svc = &Service{Store: newFakeStore(), Machines: f.vms, Clusters: fakeClusters{Cluster{ID: "c1", Provider: "ovh", Region: "eu-west-gra"}},
		Backends: []domain.VPCBackend{f.backend},
		RegionOf: func(v *computeDomain.VPS) string { return "eu-west-" + v.Location },
		Now:      func() time.Time { return f.now }}
	return f
}

func (f *fixture) vps(name, state string) *computeDomain.VPS {
	v := &computeDomain.VPS{ID: uuid.New(), OrganizationID: f.org, ProjectID: f.project, Name: name, Location: "gra",
		Provider: "ovh", State: state, IPv4: "203.0.113.5"}
	f.vms[v.ID] = v
	return v
}

func TestAttach_Native(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	a, b := f.vps("web", computeDomain.StateRunning), f.vps("Batch", computeDomain.StateStopped)

	r, err := f.svc.Attach(ctx, f.org, a.ID, "u1")
	require.NoError(t, err)
	assert.Nil(t, r.Enrollment, "réseau natif : rien à faire sur le VPS")
	assert.Equal(t, domain.StateAttached, r.Member.State)
	assert.Equal(t, "10.248.0.2", r.Member.Address)
	assert.Equal(t, "web.internal", r.Member.InternalName)

	r2, err := f.svc.Attach(ctx, f.org, b.ID, "u1")
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.3", r2.Member.Address)
	assert.Equal(t, "batch.internal", r2.Member.InternalName)

	again, err := f.svc.Attach(ctx, f.org, a.ID, "u1")
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.2", again.Member.Address, "idempotent : même adresse")

	view, err := f.svc.Get(ctx, f.project)
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	assert.Equal(t, "10.248.0.0/24", view.Range)
	require.Len(t, view.Members, 2)
	assert.Equal(t, "Batch", view.Members[0].Name, "triés par nom")
	require.NotNil(t, view.Members[1].Connected)
	assert.True(t, *view.Members[1].Connected)
}

func TestAttach_Enrollment(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)

	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	require.NotNil(t, r.Enrollment)
	assert.True(t, strings.HasPrefix(r.Enrollment.Token, tokenPrefix))
	assert.Equal(t, f.now.Add(domain.EnrollmentTTL), r.Enrollment.ExpiresAt)
	assert.Equal(t, domain.StatePending, r.Member.State)

	// Nouvelle demande : nouveau jeton, l'ancien ne vaut plus.
	r2, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	require.NotNil(t, r2.Enrollment)
	assert.NotEqual(t, r.Enrollment.Token, r2.Enrollment.Token)
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="})
	assert.ErrorIs(t, err, domain.ErrInvalidEnrollment)

	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r2.Enrollment.Token, PublicKey: "pas une clé"})
	assert.ErrorIs(t, err, domain.ErrInvalidPublicKey, "clé refusée avant de consommer le jeton")

	out, err := f.svc.Enroll(ctx, EnrollInput{Token: r2.Enrollment.Token, PublicKey: "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="})
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.2", out.Address)
	assert.Equal(t, "10.248.0.0/24", out.Range)
	assert.Equal(t, ".internal", out.Suffix)
	assert.NotEmpty(t, out.Config)

	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r2.Enrollment.Token, PublicKey: "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="})
	assert.ErrorIs(t, err, domain.ErrInvalidEnrollment, "usage unique")

	r3, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	assert.Nil(t, r3.Enrollment, "relié : plus de jeton")
	assert.Equal(t, domain.StateAttached, r3.Member.State)
}

func TestEnroll_Expired(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	f.now = f.now.Add(domain.EnrollmentTTL + time.Second)
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="})
	assert.ErrorIs(t, err, domain.ErrInvalidEnrollment)
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: "autre-chose", PublicKey: "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="})
	assert.ErrorIs(t, err, domain.ErrInvalidEnrollment)
}

func TestAttach_Refusals(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	_, err := f.svc.Attach(ctx, f.org, f.vps("p", computeDomain.StatePending).ID, "u1")
	assert.ErrorIs(t, err, domain.ErrVPSNotReady)
	_, err = f.svc.Attach(ctx, f.org, f.vps("s", computeDomain.StateSuspended).ID, "u1")
	assert.ErrorIs(t, err, domain.ErrVPSNotReady)
	_, err = f.svc.Attach(ctx, f.org, f.vps("d", computeDomain.StateDeleted).ID, "u1")
	assert.ErrorIs(t, err, computeDomain.ErrVPSNotFound)
	_, err = f.svc.Attach(ctx, uuid.New(), f.vps("o", computeDomain.StateRunning).ID, "u1")
	assert.ErrorIs(t, err, computeDomain.ErrVPSNotFound, "autre organisation")

	far := f.vps("far", computeDomain.StateRunning)
	far.Location = "sbg"
	_, err = f.svc.Attach(ctx, f.org, far.ID, "u1")
	assert.ErrorIs(t, err, domain.ErrRegionMismatch)

	f.svc.Backends = nil
	_, err = f.svc.Attach(ctx, f.org, f.vps("n", computeDomain.StateRunning).ID, "u1")
	assert.ErrorIs(t, err, domain.ErrUnavailable)

	view, err := f.svc.Get(ctx, f.project)
	require.NoError(t, err)
	assert.False(t, view.Enabled, "aucun refus ne crée de VPC")
	assert.Empty(t, view.Members)
}

func TestDetach(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	a, b := f.vps("a", computeDomain.StateRunning), f.vps("b", computeDomain.StateRunning)

	assert.ErrorIs(t, f.svc.Detach(ctx, f.org, a.ID), domain.ErrNotAttached)
	_, err := f.svc.Attach(ctx, f.org, a.ID, "u1")
	require.NoError(t, err)
	_, err = f.svc.Attach(ctx, f.org, b.ID, "u1")
	require.NoError(t, err)

	require.NoError(t, f.svc.Detach(ctx, f.org, a.ID))
	view, err := f.svc.Get(ctx, f.project)
	require.NoError(t, err)
	require.Len(t, view.Members, 1)
	assert.Equal(t, "b", view.Members[0].Name)

	require.NoError(t, f.svc.Detach(ctx, f.org, b.ID))
	st, err := f.backend.Status(ctx, domain.VPC{ProjectID: f.project, ClusterID: "c1"})
	require.NoError(t, err)
	assert.Empty(t, st, "dernier VPS parti : réseau démonté")

	// Plage gardée par le projet ; l'adresse libérée est redonnée.
	r, err := f.svc.Attach(ctx, f.org, b.ID, "u1")
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.2", r.Member.Address)
	view, err = f.svc.Get(ctx, f.project)
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.0/24", view.Range)
}

// Plusieurs projets du même cluster : plages distinctes.
func TestAttach_RangesPerProject(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	_, err := f.svc.Attach(ctx, f.org, f.vps("a", computeDomain.StateRunning).ID, "u1")
	require.NoError(t, err)
	other := f.project
	f.project = uuid.New()
	r, err := f.svc.Attach(ctx, f.org, f.vps("b", computeDomain.StateRunning).ID, "u1")
	require.NoError(t, err)
	assert.Equal(t, "10.248.1.2", r.Member.Address)
	v1, _ := f.svc.Get(ctx, other)
	v2, _ := f.svc.Get(ctx, f.project)
	assert.NotEqual(t, v1.Range, v2.Range)
}
