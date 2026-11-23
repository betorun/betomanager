package repository_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/repository"
)

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	dsn := os.Getenv("IAM_E2E_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:pw@127.0.0.1:55452/postgres?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

func TestE2E_Registry(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	store := &repository.RegistryStore{DB: db}
	suffix := uuid.NewString()[:8]
	cfgID, other := "cfg-"+suffix, "prox-"+suffix

	// Le cluster de la configuration devient le défaut (s'il n'y en a pas).
	require.NoError(t, db.Exec(`UPDATE clusters SET is_default = false`).Error)
	require.NoError(t, store.EnsureDefault(ctx, cfgID))
	require.NoError(t, store.EnsureDefault(ctx, cfgID)) // idempotent
	c, err := store.Get(ctx, cfgID)
	require.NoError(t, err)
	assert.True(t, c.IsDefault)
	assert.Equal(t, domain.ProviderOther, c.Provider)
	assert.Nil(t, c.EnrolledAt, "déclaré, pas encore enrôlé")

	// Déclaré puis enrôlé par son watcher : la déclaration reste.
	require.NoError(t, store.Declare(ctx, &domain.Cluster{ClusterID: other, Name: "Proxmox GRA", Provider: domain.ProviderProxmox,
		Distribution: domain.DistributionK8s, Region: "eu-west-gra", State: domain.StateActive}))
	assert.ErrorIs(t, store.Declare(ctx, &domain.Cluster{ClusterID: other, Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s, State: domain.StateActive}), domain.ErrClusterExists)
	enrolled, err := repository.NewClusterRepository(db).Upsert(ctx, other, "v1.2.0")
	require.NoError(t, err)
	assert.NotNil(t, enrolled.EnrolledAt)
	assert.Equal(t, "Proxmox GRA", enrolled.Name)
	assert.Equal(t, domain.ProviderProxmox, enrolled.Provider)

	// Un seul défaut ; le défaut ne se retire pas.
	require.NoError(t, store.SetDefault(ctx, other))
	var defaults int64
	require.NoError(t, db.Model(&domain.Cluster{}).Where("is_default").Count(&defaults).Error)
	assert.EqualValues(t, 1, defaults)
	assert.ErrorIs(t, store.Delete(ctx, other), domain.ErrClusterNotFound)

	// Santé : remplacée d'un bloc, partie avec le cluster.
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.SaveHealth(ctx, &domain.Health{ClusterID: cfgID, State: domain.HealthDown, Reasons: []string{domain.ReasonAPIUnreachable}, CheckedAt: now, Since: now}))
	require.NoError(t, store.SaveHealth(ctx, &domain.Health{ClusterID: cfgID, State: domain.HealthOK, Reasons: []string{}, NodesTotal: 3, CheckedAt: now, Since: now}))
	hs, err := store.Healths(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.HealthOK, hs[cfgID].State)
	assert.Equal(t, 3, hs[cfgID].NodesTotal)
	require.NoError(t, store.Delete(ctx, cfgID))
	hs, err = store.Healths(ctx)
	require.NoError(t, err)
	assert.NotContains(t, hs, cfgID)

	// Accès chiffré stocké tel quel, jamais en JSON.
	require.NoError(t, store.Update(ctx, other, map[string]any{"kubeconfig_ciphertext": []byte{1, 2, 3}, "api_server": "https://10.0.0.1:6443"}))
	c, err = store.Get(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3}, c.KubeconfigCiphertext)
	require.NoError(t, db.Exec(`UPDATE clusters SET is_default = false WHERE cluster_id = ?`, other).Error)
	require.NoError(t, store.Delete(ctx, other))
}

// Projets comptés par cluster (sans cluster : sous le défaut) ; la base
// refuse de retirer un cluster qui porte un projet.
func TestE2E_ProjectCounts(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	store := &repository.RegistryStore{DB: db}
	suffix := uuid.NewString()[:8]
	host := "host-" + suffix
	require.NoError(t, store.Declare(ctx, &domain.Cluster{ClusterID: host, Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s, State: domain.StateActive}))
	org := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e placement')`, org).Error)
	p1, p2 := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id, cluster_id) VALUES (?, ?, ?, ?, ?)`,
		p1, "pl-"+suffix, "km-a"+suffix, org, host).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		p2, "pl2-"+suffix, "km-b"+suffix, org).Error)
	counts, err := store.ProjectCounts(ctx, "le-defaut-"+suffix)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[host])
	assert.GreaterOrEqual(t, counts["le-defaut-"+suffix], 1, "projet sans cluster : compté sous le défaut")
	assert.Error(t, store.Delete(ctx, host), "clé étrangère : le cluster porte un projet")
	require.NoError(t, db.Exec(`DELETE FROM projects WHERE id IN (?, ?)`, p1, p2).Error)
	require.NoError(t, store.Delete(ctx, host))
}
