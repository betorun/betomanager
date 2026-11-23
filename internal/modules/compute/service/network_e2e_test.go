package service_test

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

func TestE2E_ComputeNetworkAndTermination(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Date(2034, 2, 1, 10, 0, 0, 0, time.UTC)
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Now: func() time.Time { return now }}
	orig := service.Resolve
	t.Cleanup(func() { service.Resolve = orig })
	service.Resolve = func(_ context.Context, host string) ([]string, error) {
		if host == "mail.exemple.cg" {
			return []string{"51.68.1.2"}, nil
		}
		return []string{"203.0.113.9"}, nil
	}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e vps network')`, org).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "net-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	paid := now.AddDate(0, 0, -1) // mois fini : il serait renouvelé
	v := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: "mail", Offer: "VPS-1", Location: "gra",
		Image: "debian-12", Provider: domain.ProviderOVH, ProviderID: "vps-nt-" + uuid.NewString()[:8], State: domain.StateRunning,
		IPv4: "51.68.1.2", PaidUntil: &paid, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(v).Error)

	_, err = s.SetReverse(ctx, org, v.ID, service.ReverseRequest{Family: "v4", Reverse: "autre.exemple.cg"}, "user:dev")
	assert.ErrorIs(t, err, domain.ErrReverseNotPointing, "le nom ne pointe pas vers le VPS")
	_, err = s.SetReverse(ctx, org, v.ID, service.ReverseRequest{Family: "v6", Reverse: "mail.exemple.cg"}, "user:dev")
	assert.ErrorIs(t, err, domain.ErrNoAddress)
	got, err := s.SetReverse(ctx, org, v.ID, service.ReverseRequest{Family: "v4", Reverse: "Mail.Exemple.cg."}, "user:dev")
	require.NoError(t, err)
	assert.Equal(t, domain.OpSettingReverse, got.Operation)
	var r domain.Request
	require.NoError(t, db.Where("vps_id = ? AND kind = ?", v.ID, domain.KindReverse).First(&r).Error)
	require.NoError(t, store.FinishRequest(ctx, r.ID, true, "", now))

	// Anti-spam : rien à débloquer tant que l'adresse n'est pas bloquée.
	_, err = s.UnblockSpam(ctx, v.ID, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNotSpamBlocked)

	// Résiliation : confirmation par le nom, coupé tout de suite, plus renouvelé, nom libéré.
	_, err = s.Terminate(ctx, org, v.ID, service.TerminateRequest{ConfirmName: "autre"}, "user:owner")
	assert.ErrorIs(t, err, domain.ErrInvalidName)
	got, err = s.Terminate(ctx, org, v.ID, service.TerminateRequest{ConfirmName: "mail"}, "user:owner")
	require.NoError(t, err)
	assert.Equal(t, domain.StateDeleted, got.State)
	assert.NotNil(t, got.TerminatedAt)
	due, err := store.DueVPS(ctx, now, nil, 100)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, v.ID, d.ID, "résilié : plus jamais renouvelé")
	}
	list, _ := s.List(ctx, org)
	assert.Empty(t, list, "absent de la console")
	var stops int64
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ? AND kind = ?", v.ID, domain.KindTerminate).Count(&stops).Error)
	assert.Equal(t, int64(1), stops)
	again := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: "mail", Offer: "VPS-1", Location: "gra",
		Image: "debian-12", Provider: domain.ProviderOVH, State: domain.StatePending, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return store.CreateVPSTx(tx, again) }), "nom réutilisable")
}
