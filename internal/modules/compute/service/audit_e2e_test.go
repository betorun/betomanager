package service_test

// Corrections de l'audit (argent et droits), sur un vrai Postgres.

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

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func auditSetup(t *testing.T, now time.Time) (*gorm.DB, *service.Service, *billingSvc.Service, *repository.GormStore) {
	t.Helper()
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	clock := func() time.Time { return now }
	billing := &billingSvc.Service{Store: billingRepo.New(db), Inventory: billingInventory.New(db), Ledger: billingRepo.NewLedger(db),
		Admin: billingRepo.NewAdmin(db), Phase4: billingRepo.NewPhase4(db), Postpaid: billingRepo.NewPostpaid(db), Now: clock}
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Catalogs: rates{billing}, Wallet: billing, Ordering: true, Now: clock}
	_, err = s.SaveOffer(context.Background(), service.OfferUpdate{Code: "VPS-1", Name: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649,
		CoefficientBP: 20000, RoundTo: 500, Locations: []string{"gra"}, Images: map[string]string{"ubuntu-24.04": "Ubuntu 24.04"},
		SnapshotPlanCode: "option-snapshot", SnapshotPriceCents: 120, Public: true, Position: 1}, "user:staff")
	require.NoError(t, err)
	_, err = s.SaveLimits(context.Background(), service.LimitsUpdate{MaxVPSPerOrg: 5, NewAccountDays: 30, NewAccountMaxVPS: 1, NewAccountOffers: []string{"VPS-1"}}, "user:staff")
	require.NoError(t, err)
	return db, s, billing, store
}

func auditOrg(t *testing.T, db *gorm.DB, created time.Time) (uuid.UUID, uuid.UUID) {
	t.Helper()
	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name, created_at) VALUES (?, 'e2e audit', ?)`, org, created).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "au-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	return org, project
}

func TestE2E_AuditMoneyAndRights(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	ctx := context.Background()
	now := time.Date(2036, 1, 10, 10, 0, 0, 0, time.UTC)
	db, s, billing, store := auditSetup(t, now)
	cash := func(org uuid.UUID) billingDomain.Milli {
		c, _, err := billingRepo.NewLedger(db).Balances(ctx, org)
		require.NoError(t, err)
		return c
	}

	// 1. Commandes simultanées d'un compte récent : une seule passe (quota sous verrou).
	org, project := auditOrg(t, db, now.AddDate(0, 0, -2))
	_, err := billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 100000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Order(ctx, service.OrderRequest{OrganizationID: org.String(), ProjectID: project.String(), Name: "par-" + string(rune('a'+i)),
				Offer: "VPS-1", Location: "gra", Image: "ubuntu-24.04", SSHPublicKey: testKey, MonthlyPrice: 9000}, "user:client")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else {
			assert.ErrorIs(t, e, domain.ErrNewAccountLimit)
		}
	}
	assert.Equal(t, 1, ok, "un seul VPS pour un compte récent, même en parallèle")
	assert.Equal(t, billingDomain.FromUnits(91000), cash(org), "un seul mois débité")

	// 2. Résilier pendant la création : refusé.
	list, err := s.List(ctx, org)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = s.Terminate(ctx, org, list[0].ID, service.TerminateRequest{ConfirmName: list[0].Name}, "user:client")
	assert.ErrorIs(t, err, domain.ErrNotReady)

	// 3. Option snapshot : souscription remboursée puis nouvelle souscription le même jour : débitée de nouveau.
	v := list[0]
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ?", v.ID).Update("status", domain.RequestDone).Error)
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"state": domain.StateRunning, "provider_id": "vps-au-" + v.ID.String()[:8]}))
	before := cash(org)
	_, err = s.SubscribeSnapshot(ctx, org, v.ID, service.SubscribeRequest{MonthlyPrice: 2000}, "user:client")
	require.NoError(t, err)
	var sub domain.Request
	require.NoError(t, db.Where("vps_id = ? AND kind = ?", v.ID, domain.KindSnapshotOrder).Order("created_at DESC").First(&sub).Error)
	require.NoError(t, store.FinishRequest(ctx, sub.ID, false, "refusé", now))
	// remboursement (comme FailSubscribe) puis option retirée
	var p struct{ Period string }
	require.NoError(t, db.Raw(`SELECT payload->>'period' AS period FROM compute_requests WHERE id = ?`, sub.ID).Scan(&p).Error)
	require.NoError(t, billing.RefundCompute(ctx, org, billingDomain.FromUnits(2000), p.Period+":refund", "test"))
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"snapshot_option": ""}))
	assert.Equal(t, before, cash(org))
	_, err = s.SubscribeSnapshot(ctx, org, v.ID, service.SubscribeRequest{MonthlyPrice: 2000}, "user:client")
	require.NoError(t, err)
	assert.Equal(t, before-billingDomain.FromUnits(2000), cash(org), "la nouvelle souscription est débitée")
}

func TestE2E_AuditRenewalsNeverStarve(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	ctx := context.Background()
	now := time.Date(2037, 2, 10, 10, 0, 0, 0, time.UTC)
	db, s, billing, store := auditSetup(t, now)
	org, project := auditOrg(t, db, now.AddDate(-1, 0, 0))
	_, err := billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 9000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	old := now.AddDate(0, -3, 0)
	// 60 VPS suspendus (impayés d'une autre organisation sans solde, et abus) plus anciens que le VPS actif.
	poor, poorProject := auditOrg(t, db, now.AddDate(-1, 0, 0))
	for i := 0; i < 60; i++ {
		code := domain.FailureUnpaid
		if i%2 == 0 {
			code = domain.FailureAbuse
		}
		v := &domain.VPS{ID: uuid.New(), OrganizationID: poor, ProjectID: poorProject, Name: "s" + uuid.NewString()[:8], Offer: "VPS-1",
			Location: "gra", Image: "ubuntu-24.04", Provider: domain.ProviderOVH, ProviderID: "vps-st-" + uuid.NewString()[:8],
			State: domain.StateSuspended, FailureCode: code, PaidUntil: &old, CreatedAt: old, UpdatedAt: old}
		require.NoError(t, db.Create(v).Error)
	}
	due := now.Add(-time.Hour)
	active := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: "actif", Offer: "VPS-1", Location: "gra",
		Image: "ubuntu-24.04", Provider: domain.ProviderOVH, ProviderID: "vps-ac-" + uuid.NewString()[:8], State: domain.StateRunning,
		PaidUntil: &due, MonthlyPrice: 9000, CreatedAt: old, UpdatedAt: old}
	require.NoError(t, db.Create(active).Error)

	renewed, _, err := s.RenewDue(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, renewed, 1)
	got, _ := store.VPS(ctx, active.ID)
	assert.True(t, got.PaidUntil.After(now), "le VPS actif est renouvelé malgré 60 VPS suspendus plus anciens")
}

// Passe B : une seule réconciliation à la fois, purge, e-mails sans doublon.
func TestE2E_AuditLoad(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	ctx := context.Background()
	now := time.Date(2036, 2, 10, 10, 0, 0, 0, time.UTC)
	db, _, _, store := auditSetup(t, now)

	// Deux passes lancées ensemble : une seule commence.
	provider := "e2e-" + uuid.NewString()[:8]
	var started int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.BeginRun(ctx, &domain.ReconcileRun{ID: uuid.New(), Provider: provider, StartedAt: now}, 30*time.Minute)
			assert.NoError(t, err)
			if ok {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, started)
	// Une passe morte (non close depuis longtemps) ne bloque plus.
	ok, err := store.BeginRun(ctx, &domain.ReconcileRun{ID: uuid.New(), Provider: provider, StartedAt: now.Add(time.Hour)}, 30*time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)

	// Même e-mail rangé deux fois : une ligne.
	fp := "fp-" + uuid.NewString()
	for i := 0; i < 2; i++ {
		require.NoError(t, store.SaveProviderMail(ctx, &domain.ProviderMail{ReceivedAt: now, Subject: "incident", Kind: domain.MailIncident, Fingerprint: fp, CreatedAt: now}))
	}
	var mails int64
	require.NoError(t, db.Model(&domain.ProviderMail{}).Where("fingerprint = ?", fp).Count(&mails).Error)
	assert.EqualValues(t, 1, mails)

	// Purge : passe ancienne effacée, récente gardée.
	old, recent := uuid.New(), uuid.New()
	require.NoError(t, store.SaveRun(ctx, &domain.ReconcileRun{ID: old, Provider: provider, StartedAt: now.AddDate(0, 0, -40)}))
	require.NoError(t, store.SaveRun(ctx, &domain.ReconcileRun{ID: recent, Provider: provider, StartedAt: now}))
	require.NoError(t, store.Purge(ctx, now, now.AddDate(0, 0, -90), now.AddDate(0, 0, -30)))
	var left []uuid.UUID
	require.NoError(t, db.Model(&domain.ReconcileRun{}).Where("id IN ?", []uuid.UUID{old, recent}).Pluck("id", &left).Error)
	assert.Equal(t, []uuid.UUID{recent}, left)
}
