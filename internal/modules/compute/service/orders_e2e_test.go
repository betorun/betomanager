package service_test

// Commande et renouvellement des VPS sur un vrai Postgres, avec la vraie
// facturation. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/compute/... -count=1

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

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA74/lOopHYYorBomTQtkD2FGCfu4OIN/VlNbSYUgYqW test@exemple"

type rates struct{ b *billingSvc.Service }

func (r rates) Rate(ctx context.Context, org uuid.UUID) (domain.Rate, error) {
	rate, round, currency, err := r.b.AccountCatalogRate(ctx, org)
	return domain.Rate{Currency: currency, EURMilli: rate, RoundTo: round}, err
}

func TestE2E_ComputeOrdersAndRenewals(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Date(2030, 1, 31, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	billing := &billingSvc.Service{Store: billingRepo.New(db), Inventory: billingInventory.New(db), Ledger: billingRepo.NewLedger(db),
		Admin: billingRepo.NewAdmin(db), Phase4: billingRepo.NewPhase4(db), Postpaid: billingRepo.NewPostpaid(db), Now: clock}
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Catalogs: rates{billing}, Wallet: billing, Ordering: true, Now: clock}

	org, project, other := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e vps'), (?, 'e2e autre')`, org, other).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "vps-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	// L'équipe tarife VPS-1 : 6,49 € → 9 000 FCFA.
	_, err = s.SaveOffer(ctx, service.OfferUpdate{Code: "VPS-1", Name: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649,
		CoefficientBP: 20000, RoundTo: 500, Locations: []string{"gra"}, Images: map[string]string{"ubuntu-24.04": "Ubuntu 24.04"}, Public: true, Position: 1}, "user:staff")
	require.NoError(t, err)
	order := service.OrderRequest{OrganizationID: org.String(), ProjectID: project.String(), Name: "web", Offer: "vps-1",
		Location: "GRA", Image: "ubuntu-24.04", SSHPublicKey: testKey, MonthlyPrice: 9000}
	cash := func() billingDomain.Milli {
		c, _, err := billingRepo.NewLedger(db).Balances(ctx, org)
		require.NoError(t, err)
		return c
	}
	requests := func(kind string) []domain.Request {
		var rs []domain.Request
		require.NoError(t, db.Where("organization_id = ? AND kind = ?", org, kind).Order("created_at").Find(&rs).Error)
		return rs
	}

	// Sans solde : refusé, rien n'est créé (transaction annulée).
	_, err = s.Order(ctx, order, "user:client")
	assert.ErrorIs(t, err, billingDomain.ErrInsufficientFunds)
	list, err := s.List(ctx, org)
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.Empty(t, requests(domain.KindCreate))

	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 20000, Reason: "test"}, "user:staff")
	require.NoError(t, err)

	// Refus avant tout débit.
	bad := order
	bad.MonthlyPrice = 8500
	_, err = s.Order(ctx, bad, "user:client")
	assert.ErrorIs(t, err, domain.ErrPriceChanged)
	bad = order
	bad.Location = "sbg"
	_, err = s.Order(ctx, bad, "user:client")
	assert.ErrorIs(t, err, domain.ErrNotSold)
	bad = order
	bad.SSHPublicKey = "ssh-rsa AAAA"
	_, err = s.Order(ctx, bad, "user:client")
	assert.ErrorIs(t, err, domain.ErrInvalidSSHKey)
	bad = order
	bad.OrganizationID = other.String()
	_, err = s.Order(ctx, bad, "user:client")
	assert.ErrorIs(t, err, domain.ErrProjectNotInOrganization, "projet d'une autre organisation")
	assert.Equal(t, billingDomain.FromUnits(20000), cash())

	// Commande : un mois débité, VPS en attente, demande de création déposée.
	v, err := s.Order(ctx, order, "user:client")
	require.NoError(t, err)
	assert.Equal(t, domain.StatePending, v.State)
	assert.Equal(t, int64(9000), v.MonthlyPrice)
	assert.Equal(t, time.Date(2030, 2, 28, 10, 0, 0, 0, time.UTC), v.PaidUntil.UTC())
	assert.Equal(t, billingDomain.FromUnits(11000), cash())
	creates := requests(domain.KindCreate)
	require.Len(t, creates, 1)
	var p domain.CreatePayload
	require.NoError(t, json.Unmarshal(creates[0].Payload, &p))
	assert.Equal(t, int64(9_000_000), p.AmountMilli)
	assert.NotContains(t, p.SSHPublicKey, "test@exemple", "clé normalisée")
	_, err = s.Order(ctx, order, "user:client")
	assert.ErrorIs(t, err, domain.ErrNameTaken)
	assert.Equal(t, billingDomain.FromUnits(11000), cash(), "nom pris : rien débité")

	// Échec de livraison : remboursé une seule fois.
	require.NoError(t, billing.RefundCompute(ctx, org, billingDomain.Milli(p.AmountMilli), p.Period+":refund", "test"))
	require.NoError(t, billing.RefundCompute(ctx, org, billingDomain.Milli(p.AmountMilli), p.Period+":refund", "test"))
	assert.Equal(t, billingDomain.FromUnits(20000), cash())
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { // débit rejoué
		written, err := billing.ChargeComputeTx(ctx, tx, org, billingDomain.Milli(p.AmountMilli), p.Period, "rejoué", "test")
		assert.False(t, written)
		return err
	}))
	assert.Equal(t, billingDomain.FromUnits(20000), cash(), "référence déjà débitée : rien")

	// Livré et en marche (le workflow le fait en vrai).
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"state": domain.StateRunning, "provider_id": "vps-e2e-" + v.ID.String()[:8]}))
	require.NoError(t, store.FinishRequest(ctx, creates[0].ID, true, "", now))

	// Fin du mois : renouvelé (le mois suivant commence le 28 février).
	now = time.Date(2030, 2, 28, 11, 0, 0, 0, time.UTC)
	renewed, suspended, err := s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, renewed)
	assert.Equal(t, 0, suspended)
	got, _ := store.VPS(ctx, v.ID)
	assert.Equal(t, time.Date(2030, 3, 28, 10, 0, 0, 0, time.UTC), got.PaidUntil.UTC())
	assert.Equal(t, billingDomain.FromUnits(11000), cash())
	renewed, _, _ = s.RenewDue(ctx)
	assert.Equal(t, 0, renewed, "rien de dû")

	// Mois suivant : 11 000 suffit encore ; puis solde insuffisant → suspendu + arrêt demandé.
	now = time.Date(2030, 3, 28, 11, 0, 0, 0, time.UTC)
	_, _, err = s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, billingDomain.FromUnits(2000), cash())
	now = time.Date(2030, 4, 28, 11, 0, 0, 0, time.UTC)
	renewed, suspended, err = s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, renewed)
	assert.Equal(t, 1, suspended)
	got, _ = store.VPS(ctx, v.ID)
	assert.Equal(t, domain.StateSuspended, got.State)
	assert.Equal(t, domain.FailureUnpaid, got.FailureCode)
	assert.Len(t, requests(domain.KindStop), 1)
	_, suspended, _ = s.RenewDue(ctx)
	assert.Equal(t, 0, suspended, "déjà suspendu : pas de second arrêt")
	assert.Len(t, requests(domain.KindStop), 1)
	require.NoError(t, store.FinishRequest(ctx, requests(domain.KindStop)[0].ID, true, "", now)) // arrêté

	// Recharge deux mois plus tard : un seul mois débité (la suspension n'est
	// pas facturée), qui commence au paiement ; relance demandée.
	now = time.Date(2030, 6, 30, 9, 0, 0, 0, time.UTC)
	_, err = billing.Adjust(ctx, org, billingDomain.AdjustmentRequest{Amount: 10000, Reason: "test"}, "user:staff")
	require.NoError(t, err)
	renewed, _, err = s.RenewDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, renewed)
	assert.Equal(t, billingDomain.FromUnits(3000), cash())
	got, _ = store.VPS(ctx, v.ID)
	assert.Equal(t, time.Date(2030, 7, 30, 9, 0, 0, 0, time.UTC), got.PaidUntil.UTC())
	assert.Len(t, requests(domain.KindStart), 1)
}
