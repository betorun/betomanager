package repository_test

// Sur un vrai Postgres (migrations complètes). Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/billing/... -count=1 -v

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

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	"github.com/thekrauss/kubemanager/internal/modules/billing/service"
)

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

// e2e : organisation neuve avec un projet, un conteneur en marche, un arrêté
// avec disque, une base et un build terminé.
func e2e(t *testing.T) (*gorm.DB, uuid.UUID, uuid.UUID) {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	org, project := uuid.New(), uuid.New()
	exec := func(q string, args ...any) { require.NoError(t, db.Exec(q, args...).Error, q) }
	exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e')`, org)
	exec(`INSERT INTO projects (id, name, organization_id) VALUES (?, ?, ?)`, project, "e2e-"+project.String()[:8], org)
	exec(`INSERT INTO workloads (project_id, name, namespace, chart_name, version, image, status, replicas, cpu_limit, memory_limit)
		VALUES (?, 'api', 'ns', 'standard-app', '1', 'img', 'RUNNING', 2, '500m', '512Mi')`, project)
	exec(`INSERT INTO workloads (project_id, name, namespace, chart_name, version, image, status, replicas, cpu_limit, memory_limit, persistence_enabled, storage_size)
		VALUES (?, 'worker', 'ns', 'standard-app', '1', 'img', 'STOPPED', 1, '250m', '256Mi', true, '2Gi')`, project)
	exec(`INSERT INTO workloads (project_id, name, namespace, chart_name, version, image, status, replicas)
		VALUES (?, 'gone', 'ns', 'standard-app', '1', 'img', 'DELETED', 1)`, project)
	exec(`INSERT INTO databases (id, project_id, name, storage_size, status) VALUES (?, ?, 'db', '5Gi', 'READY')`, uuid.New(), project)
	now := time.Now().UTC()
	exec(`INSERT INTO builds (id, project_id, name, source_type, status, image, started_at, finished_at)
		VALUES (?, ?, 'api', 'git', 'SUCCEEDED', 'img', ?, ?)`, uuid.New(), project, now.Add(-7*time.Minute-10*time.Second), now.Add(-time.Minute))
	return db, org, project
}

func TestE2E_SampleAndEstimate(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	store := repository.New(db)
	s := &service.Service{Store: store, Inventory: inventory.New(db)}

	n, err := s.Sample(ctx)
	require.NoError(t, err)
	assert.Positive(t, n)
	_, err = s.Sample(ctx) // même heure : aucune ligne en double
	require.NoError(t, err)

	var rows []domain.Usage
	require.NoError(t, db.Where("organization_id = ?", org).Find(&rows).Error)
	byMeter := map[domain.Meter]domain.Usage{}
	for _, r := range rows {
		byMeter[r.Meter] = r
	}
	require.Len(t, rows, 5, "calcul de api, disque du worker arrêté, base, disque de la base, build")
	assert.Equal(t, "small", byMeter[domain.MeterCompute].Size)
	assert.Equal(t, int64(2), byMeter[domain.MeterCompute].Quantity)
	assert.Equal(t, int64(2), byMeter[domain.MeterDisk].Quantity)
	assert.Equal(t, int64(5), byMeter[domain.MeterDatabaseDisk].Quantity)
	assert.Equal(t, int64(7), byMeter[domain.MeterBuildMinutes].Quantity, "minutes commencées")
	assert.Equal(t, "1", byMeter[domain.MeterCompute].CatalogVersion, "grille amorcée par la migration")

	e, err := s.Estimate(ctx, org, "")
	require.NoError(t, err)
	assert.Equal(t, "XAF", e.Currency)
	require.Len(t, e.Projects, 1)
	assert.Equal(t, project.String(), e.Projects[0].ProjectID)
	assert.Positive(t, e.Gross)
	assert.Positive(t, e.Projected)
}

func TestE2E_CatalogVersions(t *testing.T) {
	db, _, _ := e2e(t)
	ctx := context.Background()
	store := repository.New(db)
	c, err := store.ActiveCatalog(ctx, "XOF")
	require.NoError(t, err)
	before := c.Version

	c.Sizes[0].Monthly = domain.FromUnits(1000)
	saved, err := store.SaveCatalog(ctx, c, "user:e2e")
	require.NoError(t, err)
	assert.NotEqual(t, before, saved.Version)

	active, err := store.ActiveCatalog(ctx, "XOF")
	require.NoError(t, err)
	assert.Equal(t, saved.Version, active.Version)
	assert.Equal(t, domain.FromUnits(1000), active.Sizes[0].Monthly)

	all, err := store.ActiveCatalogs(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(all), 2, "XAF et XOF")

	_, err = store.ActiveCatalog(ctx, "EUR")
	assert.ErrorIs(t, err, domain.ErrUnknownCurrency)
}
