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

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
	legalContract "github.com/thekrauss/kubemanager/internal/modules/legal/contract"
)

type termsGate struct {
	err   error
	kinds []string
}

func (g *termsGate) RequireAccepted(_ context.Context, _ uuid.UUID, kinds ...string) error {
	g.kinds = kinds
	return g.err
}

func TestE2E_ComputeSafeguards(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Date(2035, 3, 1, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	billing := &billingSvc.Service{Store: billingRepo.New(db), Inventory: billingInventory.New(db), Ledger: billingRepo.NewLedger(db),
		Admin: billingRepo.NewAdmin(db), Phase4: billingRepo.NewPhase4(db), Postpaid: billingRepo.NewPostpaid(db), Now: clock}
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Catalogs: rates{billing}, Wallet: billing, Ordering: true, Now: clock}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name, created_at) VALUES (?, 'e2e vps garde-fous', ?)`, org, now.AddDate(0, 0, -3)).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "sg-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	for _, code := range []string{"VPS-1", "VPS-2"} {
		_, err = s.SaveOffer(ctx, service.OfferUpdate{Code: code, Name: code, ProviderPlanCode: "plan-" + code, ProviderPriceCents: 649,
			CoefficientBP: 20000, RoundTo: 500, Locations: []string{"gra", "rbx"}, Images: map[string]string{"ubuntu-24.04": "Ubuntu 24.04"}, Public: true}, "user:staff")
		require.NoError(t, err)
	}
	_, err = s.SaveLimits(ctx, service.LimitsUpdate{MaxVPSPerOrg: 3, NewAccountDays: 30, NewAccountMaxVPS: 1, NewAccountOffers: []string{"vps-1"}}, "user:staff")
	require.NoError(t, err)
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 100000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	order := func(name, offer, loc string) error {
		_, err := s.Order(ctx, service.OrderRequest{OrganizationID: org.String(), ProjectID: project.String(), Name: name, Offer: offer,
			Location: loc, Image: "ubuntu-24.04", SSHPublicKey: testKey, MonthlyPrice: 9000}, "user:client")
		return err
	}

	// Conditions non acceptées : refusé avant tout débit.
	gate := &termsGate{err: legalContract.ErrTermsNotAccepted}
	s.Terms = gate
	assert.ErrorIs(t, order("a", "VPS-1", "gra"), legalContract.ErrTermsNotAccepted)
	assert.Equal(t, []string{legalContract.KindTermsVPS, legalContract.KindAUP}, gate.kinds)
	gate.err = nil

	// Rupture relevée à Roubaix : refusée avant tout débit ; affichée dans les offres.
	require.NoError(t, store.ReplaceStock(ctx, "VPS-1", []domain.Stock{{Offer: "VPS-1", Location: "rbx", Status: domain.StockOutOfStock, CheckedAt: now},
		{Offer: "VPS-1", Location: "gra", Status: domain.StockAvailable, CheckedAt: now}}))
	assert.ErrorIs(t, order("a", "VPS-1", "rbx"), domain.ErrOutOfStock)
	offers, err := s.Offers(ctx, org)
	require.NoError(t, err)
	for _, o := range offers {
		if o.Code == "VPS-1" {
			assert.Equal(t, domain.StockOutOfStock, o.Stock["rbx"].Status)
		}
	}

	// Compte récent (3 jours) : VPS-1 seulement, un seul.
	assert.ErrorIs(t, order("a", "VPS-2", "gra"), domain.ErrNewAccountLimit)
	require.NoError(t, order("a", "VPS-1", "gra"))
	assert.ErrorIs(t, order("b", "VPS-1", "gra"), domain.ErrNewAccountLimit)

	// L'équipe dispense l'organisation : quota de la plateforme (3).
	view, err := s.SetOrgLimits(ctx, org, service.OrgLimitsUpdate{Trusted: true}, "user:staff")
	require.NoError(t, err)
	assert.False(t, view.Recent)
	require.NoError(t, order("b", "VPS-2", "gra"))
	require.NoError(t, order("c", "VPS-1", "gra"))
	assert.ErrorIs(t, order("d", "VPS-1", "gra"), domain.ErrQuotaReached)
	two := 4
	_, err = s.SetOrgLimits(ctx, org, service.OrgLimitsUpdate{MaxVPS: &two, Trusted: true}, "user:staff")
	require.NoError(t, err)
	require.NoError(t, order("d", "VPS-1", "gra"))

	// Abus : suspendu (motif gardé), puis rétabli.
	list, err := s.List(ctx, org)
	require.NoError(t, err)
	v := list[0]
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ?", v.ID).Update("status", domain.RequestDone).Error)
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"state": domain.StateRunning, "provider_id": "vps-sg-" + v.ID.String()[:8]}))
	got, err := s.SuspendForAbuse(ctx, v.ID, "Envoi de spam signalé", "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.StateSuspended, got.State)
	assert.Equal(t, domain.FailureAbuse, got.FailureCode)
	assert.Equal(t, "Envoi de spam signalé", got.AbuseNote)
	_, err = s.Act(ctx, org, v.ID, service.ActionStart, nil, "user:client")
	assert.ErrorIs(t, err, domain.ErrSuspended, "le client ne relance pas un VPS suspendu pour abus")
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ?", v.ID).Update("status", domain.RequestDone).Error)
	got, err = s.ResumeFromAbuse(ctx, v.ID, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "", got.AbuseNote)
	var starts int64
	require.NoError(t, db.Model(&domain.Request{}).Where("vps_id = ? AND kind = ? AND status = ?", v.ID, domain.KindStart, domain.RequestPending).Count(&starts).Error)
	assert.Equal(t, int64(1), starts, "relancé")

	// Rattachement d'un VPS livré : seulement à un orphelin du relevé.
	w := list[1]
	_, err = s.AttachDelivered(ctx, w.ID, "vps-inconnu")
	assert.ErrorIs(t, err, domain.ErrNotOrphan)
	require.NoError(t, store.ReplaceMachines(ctx, domain.ProviderOVH, []domain.ProviderMachine{{Provider: domain.ProviderOVH,
		ProviderID: "vps-orphelin-" + w.ID.String()[:8], State: domain.StateRunning, Drift: domain.DriftOrphan, SeenAt: now}}))
	got, err = s.AttachDelivered(ctx, w.ID, "vps-orphelin-"+w.ID.String()[:8])
	require.NoError(t, err)
	assert.Equal(t, "vps-orphelin-"+w.ID.String()[:8], got.ProviderID)

	// E-mails du fournisseur : rangés, puis traités.
	require.NoError(t, store.SaveProviderMail(ctx, &domain.ProviderMail{ReceivedAt: now, Subject: "[ABUSE] spam", Kind: domain.MailAbuse, VPSID: &v.ID, CreatedAt: now}))
	mails, err := s.ProviderMails(ctx, true)
	require.NoError(t, err)
	require.NotEmpty(t, mails)
	require.NoError(t, s.HandleProviderMail(ctx, mails[0].ID, "user:staff"))
	again, _ := s.ProviderMails(ctx, true)
	for _, m := range again {
		assert.NotEqual(t, mails[0].ID, m.ID)
	}
	_ = gorm.ErrRecordNotFound
}
