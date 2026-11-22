package service_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func TestE2E_ComputeBackups(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Date(2032, 3, 10, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	billing := &billingSvc.Service{Store: billingRepo.New(db), Inventory: billingInventory.New(db), Ledger: billingRepo.NewLedger(db),
		Admin: billingRepo.NewAdmin(db), Phase4: billingRepo.NewPhase4(db), Postpaid: billingRepo.NewPostpaid(db), Now: clock}
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Catalogs: rates{billing}, Wallet: billing, Ordering: true, Now: clock}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e vps backups')`, org).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "bkp-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	// VPS-1 : 9 000 FCFA ; option snapshot 1,20 € × 655,957 × 2 = 1 574 → 2 000 FCFA.
	_, err = s.SaveOffer(ctx, service.OfferUpdate{Code: "VPS-1", Name: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649,
		CoefficientBP: 20000, RoundTo: 500, Locations: []string{"gra"}, Images: map[string]string{"ubuntu-24.04": "Ubuntu 24.04"},
		SnapshotPlanCode: "option-snapshot", SnapshotPriceCents: 120, Public: true, Position: 1}, "user:staff")
	require.NoError(t, err)
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 12000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	cash := func() billingDomain.Milli {
		c, _, err := billingRepo.NewLedger(db).Balances(ctx, org)
		require.NoError(t, err)
		return c
	}
	paid := now.AddDate(0, 0, 5)
	v := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: "web", Offer: "VPS-1", Location: "gra",
		Image: "ubuntu-24.04", Provider: domain.ProviderOVH, ProviderID: "vps-bk-" + uuid.NewString()[:8], State: domain.StateRunning,
		PaidUntil: &paid, MonthlyPrice: 9000, Currency: "XAF", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(v).Error)
	finish := func(kind string) {
		var r domain.Request
		require.NoError(t, db.Where("vps_id = ? AND kind = ? AND status = ?", v.ID, kind, domain.RequestPending).First(&r).Error)
		require.NoError(t, store.FinishRequest(ctx, r.ID, true, "", now))
	}

	// Pas encore relevé : rien à restaurer.
	_, err = s.Restore(ctx, org, v.ID, service.RestoreRequest{RestorePoint: now, Mode: "full"}, "user:ed")
	assert.ErrorIs(t, err, domain.ErrBackupsUnavailable)

	point := time.Date(2032, 3, 10, 2, 0, 0, 0, time.UTC)
	_, err = service.SaveBackups(ctx, store, v.ID, &service.ProviderBackups{Automated: true, RestorePoints: []time.Time{point}}, now)
	require.NoError(t, err)
	view, err := s.Backups(ctx, org, v.ID)
	require.NoError(t, err)
	assert.True(t, view.Automated)
	assert.Len(t, view.RestorePoints, 1)
	assert.Equal(t, int64(2000), view.SnapshotMonthly)
	assert.Equal(t, "XAF", view.Currency)

	_, err = s.Restore(ctx, org, v.ID, service.RestoreRequest{RestorePoint: point.Add(time.Hour), Mode: "full"}, "user:ed")
	assert.ErrorIs(t, err, domain.ErrRestorePointNotFound)
	got, err := s.Restore(ctx, org, v.ID, service.RestoreRequest{RestorePoint: point, Mode: "mount"}, "user:ed")
	require.NoError(t, err)
	assert.Equal(t, domain.OpMounting, got.Operation)
	var r domain.Request
	require.NoError(t, db.Where("vps_id = ? AND kind = ?", v.ID, domain.KindRestore).First(&r).Error)
	var p domain.RestorePayload
	require.NoError(t, json.Unmarshal(r.Payload, &p))
	assert.Equal(t, domain.RestoreMount, p.Mode)
	finish(domain.KindRestore)

	// Snapshot : option non souscrite → refus ; souscription : 2 000 débités.
	_, err = s.CreateSnapshot(ctx, org, v.ID, service.SnapshotRequest{Description: "x"}, "user:dev")
	assert.ErrorIs(t, err, domain.ErrSnapshotNotSubscribed)
	_, err = s.SubscribeSnapshot(ctx, org, v.ID, service.SubscribeRequest{MonthlyPrice: 1500}, "user:owner")
	assert.ErrorIs(t, err, domain.ErrPriceChanged)
	got, err = s.SubscribeSnapshot(ctx, org, v.ID, service.SubscribeRequest{MonthlyPrice: 2000}, "user:owner")
	require.NoError(t, err)
	assert.Equal(t, domain.SnapshotOrdering, got.SnapshotOption)
	assert.Equal(t, billingDomain.FromUnits(10000), cash())
	finish(domain.KindSnapshotOrder)
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"snapshot_option": domain.SnapshotActive}))

	// Un seul snapshot : le second est refusé.
	_, err = service.SaveBackups(ctx, store, v.ID, &service.ProviderBackups{Automated: true, RestorePoints: []time.Time{point}, SnapshotOption: true,
		Snapshot: &domain.SnapshotInfo{CreatedAt: now, Description: "avant"}}, now)
	require.NoError(t, err)
	_, err = s.CreateSnapshot(ctx, org, v.ID, service.SnapshotRequest{Description: "encore"}, "user:dev")
	assert.ErrorIs(t, err, domain.ErrSnapshotExists)

	// Renouvellement : VPS + option dans la même transaction (9 000 + 2 000).
	// 10 000 ne couvre pas les deux : rien n'est débité, le VPS est suspendu.
	now = paid.Add(time.Hour)
	renewed, suspended, err := s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, renewed)
	assert.Equal(t, 1, suspended)
	assert.Equal(t, billingDomain.FromUnits(10000), cash(), "ni le VPS ni l'option débités seuls")
	finish(domain.KindStop)

	// Recharge : les deux sont débités ensemble, le VPS est relancé.
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 1000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	renewed, _, err = s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, renewed)
	assert.Equal(t, billingDomain.FromUnits(0), cash())
}
