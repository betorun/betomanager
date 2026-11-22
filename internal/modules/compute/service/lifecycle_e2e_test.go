package service_test

import (
	"context"
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

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type recordedEvents struct {
	mu   sync.Mutex
	seen []string
}

func (e *recordedEvents) VPSChanged(_ context.Context, v *domain.VPS) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, v.State+"/"+v.Operation)
}

func (e *recordedEvents) BackupsChanged(_ context.Context, v *domain.VPS) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, "backups/"+v.ID.String())
}

func TestE2E_ComputeLifecycle(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Date(2031, 5, 10, 10, 0, 0, 0, time.UTC)
	store := &repository.GormStore{DB: db}
	events := &recordedEvents{}
	s := &service.Service{Store: store, Events: events, Now: func() time.Time { return now }}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e vps cycle')`, org).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "cyc-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	paid := now.AddDate(0, 0, 20)
	mk := func(name, state string) *domain.VPS {
		v := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: name, Offer: "VPS-1", Location: "gra",
			Image: "ubuntu-24.04", Provider: domain.ProviderOVH, ProviderID: "vps-" + uuid.NewString()[:8], State: state,
			PaidUntil: &paid, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, db.Create(v).Error)
		return v
	}
	web, db1 := mk("web", domain.StateRunning), mk("db", domain.StateStopped)

	// Action refusée hors de son état ; acceptée sinon, opération marquée.
	_, err = s.Act(ctx, org, web.ID, service.ActionStart, nil, "user:dev")
	assert.ErrorIs(t, err, domain.ErrNotReady, "déjà en marche")
	v, err := s.Act(ctx, org, web.ID, service.ActionReboot, nil, "user:dev")
	require.NoError(t, err)
	assert.Equal(t, domain.OpRebooting, v.Operation)
	_, err = s.Act(ctx, org, web.ID, service.ActionStop, nil, "user:dev")
	assert.ErrorIs(t, err, domain.ErrBusy, "une opération à la fois")
	_, err = s.Act(ctx, uuid.New(), web.ID, service.ActionStop, nil, "user:dev")
	assert.ErrorIs(t, err, domain.ErrVPSNotFound, "autre organisation")

	// La base elle-même refuse une seconde demande active (même sans le contrôle applicatif).
	err = db.Transaction(func(tx *gorm.DB) error {
		return store.EnqueueTx(tx, &domain.Request{OrganizationID: org, VPSID: &web.ID, Kind: domain.KindStop})
	})
	assert.ErrorIs(t, err, domain.ErrBusy)

	// Fin de l'opération : demande close, VPS libre.
	var r domain.Request
	require.NoError(t, db.Where("vps_id = ? AND status = ?", web.ID, domain.RequestPending).First(&r).Error)
	require.NoError(t, store.FinishRequest(ctx, r.ID, true, "", now))
	got, _ := store.VPS(ctx, web.ID)
	assert.Equal(t, "", got.Operation)
	_, err = s.Act(ctx, org, web.ID, service.ActionStop, nil, "user:dev")
	require.NoError(t, err, "libre à nouveau")

	// Réinstallation : système de l'offre et clé valide exigés.
	_, err = s.Act(ctx, org, db1.ID, service.ActionReinstall, &service.ReinstallRequest{Image: "windows", SSHPublicKey: testKey}, "user:ed")
	assert.ErrorIs(t, err, domain.ErrNotSold)
	_, err = s.Act(ctx, org, db1.ID, service.ActionReinstall, &service.ReinstallRequest{Image: "ubuntu-24.04", SSHPublicKey: "nope"}, "user:ed")
	assert.ErrorIs(t, err, domain.ErrInvalidSSHKey)

	// Suspension de l'organisation : web est occupé (arrêt en cours), db arrêté.
	require.NoError(t, s.SuspendOrganization(ctx, org))
	for _, id := range []uuid.UUID{web.ID, db1.ID} {
		got, _ := store.VPS(ctx, id)
		assert.Equal(t, domain.StateSuspended, got.State)
		assert.Equal(t, domain.FailureOrgSuspended, got.FailureCode)
	}
	_, err = s.Act(ctx, org, db1.ID, service.ActionStart, nil, "user:dev")
	assert.ErrorIs(t, err, domain.ErrSuspended)

	// Reprise : db (arrêté) redevient arrêté ; web (en marche) est relancé.
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ?", web.ID).Update("status", domain.RequestDone).Error)
	require.NoError(t, s.ResumeOrganization(ctx, org))
	got, _ = store.VPS(ctx, db1.ID)
	assert.Equal(t, domain.StateStopped, got.State)
	assert.Equal(t, "", got.FailureCode)
	var starts int64
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ? AND kind = ? AND status = ?", web.ID, domain.KindStart, domain.RequestPending).Count(&starts).Error)
	assert.Equal(t, int64(1), starts)
	assert.NotEmpty(t, events.seen, "changements publiés")
}
