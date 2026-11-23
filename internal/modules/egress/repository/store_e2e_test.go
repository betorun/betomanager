package repository

import (
	"context"
	"fmt"
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

	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
)

func TestE2E_EgressStore(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" || os.Getenv("IAM_E2E_DSN") == "" {
		t.Skip("IAM_E2E / IAM_E2E_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("IAM_E2E_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	s := &Store{DB: db}
	sfx := uuid.NewString()[:8]
	cluster := "egr-" + sfx
	require.NoError(t, db.Exec(`INSERT INTO clusters (cluster_id, egress_gateway) VALUES (?, 'cilium')`, cluster).Error)
	assert.Error(t, db.Exec(`UPDATE clusters SET egress_gateway = 'calico' WHERE cluster_id = ?`, cluster).Error, "passerelle inconnue refusée")

	var org uuid.UUID
	require.NoError(t, db.Raw(`SELECT id FROM organizations LIMIT 1`).Row().Scan(&org))
	projects := make([]uuid.UUID, 4)
	for i := range projects {
		projects[i] = uuid.New()
		require.NoError(t, db.Exec(`INSERT INTO projects (id, organization_id, name, namespace, cluster_id) VALUES (?, ?, ?, ?, ?)`,
			projects[i], org, "p"+sfx+string(rune('a'+i)), "km-"+sfx+string(rune('a'+i)), cluster).Error)
	}
	now := time.Now().UTC()
	// Adresses propres à ce passage (uniques dans toute la base).
	r := uuid.New()
	ipN := func(n int) string { return fmt.Sprintf("45.%d.%d.%d", r[0], r[1], n) }
	for i, ip := range []string{ipN(10), ipN(11), ipN(12)} {
		require.NoError(t, s.Declare(ctx, &domain.Address{ID: uuid.New(), ClusterID: cluster, Address: ip + "", GatewayNode: "gw-1",
			CreatedAt: now, UpdatedAt: now}), i)
	}
	assert.ErrorIs(t, s.Declare(ctx, &domain.Address{ID: uuid.New(), ClusterID: cluster, Address: ipN(10), GatewayNode: "gw-1"}), domain.ErrAddressExists)
	assert.ErrorIs(t, s.Declare(ctx, &domain.Address{ID: uuid.New(), ClusterID: "nope-" + sfx, Address: ipN(99), GatewayNode: "gw-1"}), ErrUnknownCluster)

	// Quatre projets demandent en même temps trois IP : trois servis, chacun la sienne.
	var wg sync.WaitGroup
	got := make([]*domain.Address, len(projects))
	errs := make([]error, len(projects))
	for i, p := range projects {
		wg.Add(1)
		go func(i int, p uuid.UUID) {
			defer wg.Done()
			got[i], errs[i] = s.Assign(ctx, cluster, p, "user:x", now)
		}(i, p)
	}
	wg.Wait()
	seen := map[string]bool{}
	served := 0
	for i := range projects {
		require.NoError(t, errs[i])
		if got[i] != nil {
			served++
			assert.False(t, seen[got[i].Address], "jamais deux fois la même")
			seen[got[i].Address] = true
		}
	}
	assert.Equal(t, 3, served)
	n, err := s.FreeCount(ctx, cluster)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	var holder uuid.UUID
	var held *domain.Address
	for i, a := range got {
		if a != nil {
			holder, held = projects[i], a
			break
		}
	}
	require.NoError(t, s.Declare(ctx, &domain.Address{ID: uuid.New(), ClusterID: cluster, Address: ipN(13), GatewayNode: "gw-1",
		CreatedAt: now, UpdatedAt: now}))
	_, err = s.Assign(ctx, cluster, holder, "", now)
	assert.ErrorIs(t, err, domain.ErrAlreadyHasAddress, "une seule IP par projet (index unique)")
	n, _ = s.FreeCount(ctx, cluster)
	assert.Equal(t, 1, n, "refus : l'IP reste libre")

	pl, err := s.Place(ctx, holder, "main")
	require.NoError(t, err)
	assert.Equal(t, cluster, pl.ClusterID)
	assert.Equal(t, domain.GatewayCilium, pl.Gateway)

	list, err := s.List(ctx, cluster)
	require.NoError(t, err)
	require.Len(t, list, 4)
	assigned := 0
	for _, a := range list {
		if a.Assigned {
			assigned++
			assert.NotEmpty(t, a.ProjectName)
		}
	}
	assert.Equal(t, 3, assigned)
	assert.ErrorIs(t, s.Delete(ctx, cluster, held.ID), domain.ErrAddressInUse)

	// Projet supprimé : orphelin à rendre ; sa place n'existe plus.
	require.NoError(t, db.Exec(`UPDATE projects SET deleted_at = now() WHERE id = ?`, holder).Error)
	orphans, err := s.Orphans(ctx)
	require.NoError(t, err)
	found := false
	for _, o := range orphans {
		if o.ID == held.ID {
			found = true
			assert.NotEmpty(t, o.Namespace)
		}
	}
	assert.True(t, found)
	pl, err = s.Place(ctx, holder, "main")
	require.NoError(t, err)
	assert.Nil(t, pl)

	require.NoError(t, s.Release(ctx, held.ID, now))
	cur, err := s.ForProject(ctx, holder)
	require.NoError(t, err)
	assert.Nil(t, cur)
	require.NoError(t, s.Delete(ctx, cluster, held.ID), "libre : retirable")
}
