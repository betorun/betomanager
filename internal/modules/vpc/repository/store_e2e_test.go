package repository

import (
	"context"
	"crypto/sha256"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

type e2e struct {
	db      *gorm.DB
	store   *Store
	org     uuid.UUID
	cluster string
	sfx     string
}

func setup(t *testing.T) *e2e {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" || os.Getenv("IAM_E2E_DSN") == "" {
		t.Skip("IAM_E2E / IAM_E2E_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("IAM_E2E_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	e := &e2e{db: db, store: &Store{DB: db}, sfx: uuid.NewString()[:8]}
	e.cluster = "vpc-" + e.sfx
	require.NoError(t, db.Exec(`INSERT INTO clusters (cluster_id) VALUES (?)`, e.cluster).Error)
	e.org = uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, ?)`, e.org, "vpc-"+e.sfx).Error)
	return e
}

func (e *e2e) project(t *testing.T, n int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	name := e.sfx + "-" + string(rune('a'+n))
	require.NoError(t, e.db.Exec(`INSERT INTO projects (id, organization_id, name, namespace, cluster_id) VALUES (?, ?, ?, ?, ?)`,
		id, e.org, "p"+name, "km-"+name, e.cluster).Error)
	return id
}

func (e *e2e) vps(t *testing.T, project uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, e.db.Exec(`INSERT INTO compute_vps (id, organization_id, project_id, name, offer, location, image, provider, state)
		VALUES (?, ?, ?, ?, 'VPS-1', 'gra', 'debian-12', 'ovh', 'running')`, id, e.org, project, name).Error)
	return id
}

func (e *e2e) attach(t *testing.T, project, vps uuid.UUID) (*domain.Network, *domain.Attachment, error) {
	a := &domain.Attachment{VPSID: vps, Backend: "memory", State: domain.StatePending, CreatedAt: time.Now().UTC()}
	n, err := e.store.Attach(context.Background(), project, e.cluster, a)
	return n, a, err
}

func TestE2E_VPCStore_Allocation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	p1, p2 := e.project(t, 0), e.project(t, 1)

	n1, a1, err := e.attach(t, p1, e.vps(t, p1, "web"))
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.0/24", n1.CIDR)
	assert.Equal(t, "10.248.0.2", a1.Address)
	_, a2, err := e.attach(t, p1, e.vps(t, p1, "batch"))
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.3", a2.Address)

	n2, a3, err := e.attach(t, p2, e.vps(t, p2, "web"))
	require.NoError(t, err)
	assert.Equal(t, "10.248.1.0/24", n2.CIDR, "un /24 par projet dans le cluster")
	assert.Equal(t, "10.248.1.2", a3.Address)

	_, _, err = e.attach(t, p1, a1.VPSID)
	assert.ErrorIs(t, err, domain.ErrAlreadyAttached)

	read, err := e.store.Network(ctx, p1)
	require.NoError(t, err)
	require.NotNil(t, read)
	assert.Equal(t, "10.248.0.0/24", read.CIDR)
	assert.Equal(t, e.cluster, read.ClusterID)
	none, err := e.store.Network(ctx, e.project(t, 4))
	require.NoError(t, err)
	assert.Nil(t, none)

	got, err := e.store.Attachments(ctx, p1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "10.248.0.2", got[0].Address, "adresse relue sans masque")

	left, err := e.store.Detach(ctx, a1.VPSID, p1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, left)
	_, a4, err := e.attach(t, p1, e.vps(t, p1, "later"))
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.2", a4.Address, "adresse libérée redonnée")

	// Contraintes de la migration.
	assert.Error(t, e.db.Exec(`INSERT INTO vpc_networks (project_id, cluster_id, cidr) VALUES (?, ?, '10.0.0.0/24')`, e.project(t, 2), e.cluster).Error, "hors de 10.248.0.0/13")
	assert.Error(t, e.db.Exec(`INSERT INTO vpc_networks (project_id, cluster_id, cidr) VALUES (?, ?, '10.248.0.0/24')`, e.project(t, 3), e.cluster).Error, "plage déjà prise dans le cluster")
	assert.Error(t, e.db.Exec(`UPDATE vpc_attachments SET state = 'broken' WHERE vps_id = ?`, a2.VPSID).Error, "état inconnu")
}

// Rattachements simultanés dans un même projet : adresses toutes distinctes.
func TestE2E_VPCStore_Concurrent(t *testing.T) {
	e := setup(t)
	p := e.project(t, 0)
	const n = 12
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = e.vps(t, p, "vm-"+string(rune('a'+i)))
	}
	var wg sync.WaitGroup
	addrs := make([]string, n)
	errs := make([]error, n)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, a, err := e.attach(t, p, ids[i])
			addrs[i], errs[i] = a.Address, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range ids {
		require.NoError(t, errs[i])
		assert.False(t, seen[addrs[i]], "adresse %s donnée deux fois", addrs[i])
		seen[addrs[i]] = true
		assert.True(t, netip.MustParsePrefix("10.248.0.0/24").Contains(netip.MustParseAddr(addrs[i])))
	}
}

func TestE2E_VPCStore_Enrollment(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	p := e.project(t, 0)
	_, a, err := e.attach(t, p, e.vps(t, p, "web"))
	require.NoError(t, err)
	now := time.Now().UTC()

	h1, h2 := sha256.Sum256([]byte("t1"+e.sfx)), sha256.Sum256([]byte("t2"+e.sfx))
	require.NoError(t, e.store.NewEnrollment(ctx, &domain.Enrollment{ID: uuid.New(), VPSID: a.VPSID, TokenHash: h1[:], ExpiresAt: now.Add(time.Minute), CreatedAt: now}))
	require.NoError(t, e.store.NewEnrollment(ctx, &domain.Enrollment{ID: uuid.New(), VPSID: a.VPSID, TokenHash: h2[:], ExpiresAt: now.Add(time.Minute), CreatedAt: now}))
	got, err := e.store.ConsumeEnrollment(ctx, h1[:], now)
	require.NoError(t, err)
	assert.Nil(t, got, "remplacé par le jeton suivant")

	// Deux inscriptions simultanées : une seule réussit.
	var wg sync.WaitGroup
	wins := make([]bool, 8)
	for i := range wins {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			en, err := e.store.ConsumeEnrollment(ctx, h2[:], now)
			wins[i] = err == nil && en != nil
		}(i)
	}
	wg.Wait()
	n := 0
	for _, w := range wins {
		if w {
			n++
		}
	}
	assert.Equal(t, 1, n)

	h3 := sha256.Sum256([]byte("t3" + e.sfx))
	require.NoError(t, e.store.NewEnrollment(ctx, &domain.Enrollment{ID: uuid.New(), VPSID: a.VPSID, TokenHash: h3[:], ExpiresAt: now.Add(-time.Second), CreatedAt: now}))
	got, err = e.store.ConsumeEnrollment(ctx, h3[:], now)
	require.NoError(t, err)
	assert.Nil(t, got, "expiré")

	require.NoError(t, e.store.SetState(ctx, a.VPSID, domain.StateAttached, "pubkey", now))
	after, err := e.store.Attachment(ctx, a.VPSID)
	require.NoError(t, err)
	assert.Equal(t, domain.StateAttached, after.State)
	assert.Equal(t, "pubkey", after.PublicKey)
	require.NotNil(t, after.AttachedAt)
}

func TestE2E_VPCStore_Lifecycle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	p := e.project(t, 0)
	v := e.vps(t, p, "web")
	_, a, err := e.attach(t, p, v)
	require.NoError(t, err)

	ns, err := e.store.Namespace(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, "km-"+e.sfx+"-a", ns)

	at := time.Now().UTC()
	require.NoError(t, e.store.SetState(ctx, v, domain.StateAttached, "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=", at))
	require.NoError(t, e.store.Reset(ctx, v))
	got, err := e.store.Attachment(ctx, v)
	require.NoError(t, err)
	assert.Equal(t, domain.StatePending, got.State)
	assert.Empty(t, got.PublicKey)
	assert.Nil(t, got.AttachedAt)
	assert.Equal(t, a.Address, got.Address, "adresse gardée")

	wanted := func() bool {
		var w bool
		require.NoError(t, e.db.Raw(`SELECT private_network FROM compute_vps WHERE id = ?`, v).Scan(&w).Error)
		return w
	}
	assert.False(t, wanted())
	require.NoError(t, e.store.SetWanted(ctx, v, true))
	assert.True(t, wanted())
	require.NoError(t, e.store.SetWanted(ctx, v, false))
	assert.False(t, wanted())

	list, err := e.store.Networks(ctx)
	require.NoError(t, err)
	found := false
	for _, n := range list {
		found = found || n.ProjectID == p
	}
	assert.True(t, found)
}

func TestE2E_VPCStore_IfEmpty(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	p := e.project(t, 0)
	ran := 0
	require.NoError(t, e.store.IfEmpty(ctx, p, func() error { ran++; return nil }))
	assert.Equal(t, 1, ran, "aucun VPS : exécuté")
	_, _, err := e.attach(t, p, e.vps(t, p, "web"))
	require.NoError(t, err)
	require.NoError(t, e.store.IfEmpty(ctx, p, func() error { ran++; return nil }))
	assert.Equal(t, 1, ran, "un VPS rattaché : rien démonté")
}
