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

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type liveProvider struct {
	service.ComputeProvider
	vms []service.ProviderVM
}

func (p *liveProvider) Name() string                                       { return domain.ProviderOVH }
func (p *liveProvider) List(context.Context) ([]service.ProviderVM, error) { return p.vms, nil }

func TestE2E_ComputeReconcile(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	store := &repository.GormStore{DB: db}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, ?)`, org, "Atelier Kalé").Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "vps-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	created := time.Now().Add(-time.Hour)
	vps := func(name, pid, state string) uuid.UUID {
		v := domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: name, Offer: "VPS-1", Location: "gra",
			Image: "debian-12", Provider: domain.ProviderOVH, ProviderID: pid, State: state, CreatedAt: created, UpdatedAt: created}
		require.NoError(t, db.Create(&v).Error)
		return v.ID
	}
	sfx := project.String()[:8]
	web := vps("web", "p-web-"+sfx, domain.StateRunning)
	susp := vps("old", "p-susp-"+sfx, domain.StateSuspended)
	gone := vps("gone", "p-gone-"+sfx, domain.StateRunning)

	r := &service.Reconciler{Store: store, Provider: &liveProvider{vms: []service.ProviderVM{
		{ProviderID: "p-web-" + sfx, State: domain.StateStopped, RawState: "stopped", IPv4: "51.68.1.2"},
		{ProviderID: "p-susp-" + sfx, State: domain.StateRunning, RawState: "running"},
		{ProviderID: "p-orphan-" + sfx, State: domain.StateRunning, RawState: "running", Plan: "vps-2025-model1"},
	}}}
	run, err := r.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, run.Updated)
	assert.GreaterOrEqual(t, run.Orphans, 1)

	var got domain.VPS
	require.NoError(t, db.First(&got, "id = ?", web).Error)
	assert.Equal(t, domain.StateStopped, got.State)
	assert.Equal(t, "51.68.1.2", got.IPv4)
	assert.Equal(t, int64(2), got.Version, "version augmentée")
	ok, err := store.ApplyObserved(ctx, web, 1, domain.StateRunning, "", "", time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "version périmée : rien écrit")

	fleet, err := (&service.Service{Store: store}).Fleet(ctx)
	require.NoError(t, err)
	drift := map[uuid.UUID]string{}
	for _, v := range fleet.VPS {
		if v.OrganizationID == org {
			drift[v.ID] = v.Drift
			assert.Equal(t, "Atelier Kalé", v.OrganizationName)
		}
	}
	assert.Equal(t, map[uuid.UUID]string{web: "", susp: domain.DriftState, gone: domain.DriftMissing}, drift)
	found := false
	for _, o := range fleet.Orphans {
		found = found || o.ProviderID == "p-orphan-"+sfx
	}
	assert.True(t, found, "VPS orphelin listé")
	require.NotEmpty(t, fleet.Runs)
	assert.Equal(t, run.ID, fleet.Runs[0].ID)

	// Deuxième passe : le relevé est remplacé (l'orphelin a été résilié chez le fournisseur).
	r.Provider = &liveProvider{vms: []service.ProviderVM{{ProviderID: "p-web-" + sfx, State: domain.StateStopped}}}
	_, err = r.Run(ctx)
	require.NoError(t, err)
	ms, err := store.Machines(ctx)
	require.NoError(t, err)
	assert.Len(t, ms, 1)
}
