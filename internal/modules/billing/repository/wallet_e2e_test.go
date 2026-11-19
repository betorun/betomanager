package repository_test

// Phase 3 sur un vrai Postgres (migrations complètes). Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/billing/... -count=1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	"github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/billing/xykpay"
)

// fakePayments : XYKPay simulé ; status : issue renvoyée par Get.
type fakePayments struct {
	status  string
	created int
}

func (f *fakePayments) Create(_ context.Context, _ xykpay.Charge, key string) (*xykpay.Payment, error) {
	f.created++
	return &xykpay.Payment{ID: "pay-" + key, Status: "pending", Environment: "sandbox"}, nil
}

func (f *fakePayments) Get(_ context.Context, id string) (*xykpay.Payment, error) {
	return &xykpay.Payment{ID: id, Status: f.status, Environment: "sandbox"}, nil
}

func walletService(db *gorm.DB, now *time.Time, pay *fakePayments) *service.Service {
	return &service.Service{Store: repository.New(db), Inventory: inventory.New(db), Ledger: repository.NewLedger(db),
		Admin: repository.NewAdmin(db), Payments: pay, WebhookSecret: "whsec", Phase4: repository.NewPhase4(db),
		Now: func() time.Time { return *now }}
}

func signed(t *testing.T, s *service.Service, paymentID string, now time.Time) error {
	body, err := json.Marshal(map[string]any{"type": "payment.succeeded", "data": map[string]any{"object": map[string]any{"id": paymentID}}})
	require.NoError(t, err)
	sig := fmt.Sprintf("t=%d,v1=%s", now.Unix(), xykpay.Sign("whsec", now.Unix(), body))
	return s.HandleWebhook(context.Background(), sig, body)
}

func TestE2E_LedgerIsBalancedAndAppendOnly(t *testing.T) {
	db, org, _ := e2e(t)
	ctx := context.Background()
	txID := uuid.New()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO billing_transactions (id, organization_id, kind, reference, currency) VALUES (?, ?, 'adjustment', ?, 'XAF')`,
			txID, org, "e2e-unbalanced-"+txID.String()).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO billing_ledger_entries (transaction_id, account, amount, currency) VALUES (?, 'platform:adjustments', 1000, 'XAF')`, txID).Error
	})
	require.Error(t, err, "une transaction déséquilibrée ne peut pas être validée")

	ledger := repository.NewLedger(db)
	tx, err := domain.NewTransaction(org, domain.TxAdjustment, "e2e-adj-"+uuid.NewString(), "XAF", "test", "e2e",
		map[string]domain.Milli{domain.CashAccount(org): 1000, domain.AccountAdjustments: -1000}, time.Now())
	require.NoError(t, err)
	written, err := ledger.Post(ctx, tx)
	require.NoError(t, err)
	assert.True(t, written)
	again := *tx
	again.ID = uuid.New()
	written, err = ledger.Post(ctx, &again)
	require.NoError(t, err)
	assert.False(t, written, "même référence : rien n'est écrit deux fois")

	require.Error(t, db.Exec(`UPDATE billing_ledger_entries SET amount = 1 WHERE transaction_id = ?`, tx.ID).Error, "immuable")
	require.Error(t, db.Exec(`DELETE FROM billing_transactions WHERE id = ?`, tx.ID).Error, "immuable")
}

func TestE2E_ChargingTopupAndCredits(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(10 * time.Minute)
	pay := &fakePayments{status: "pending"}
	s := walletService(db, &now, pay)

	// Avant activation : estimation seulement, pas de recharge.
	w, err := s.Wallet(ctx, org)
	require.NoError(t, err)
	assert.Equal(t, domain.WalletEstimate, w.Status)
	_, err = s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 5000, Phone: "+242061234567", Operator: "cg_mtn"}, "e2e")
	assert.ErrorIs(t, err, domain.ErrChargingDisabled)

	// Activation : plus de crédit d'essai à l'activation (il vient avec la
	// première recharge, une fois par numéro).
	activatedAt := now.Add(-3 * time.Hour)
	now = activatedAt
	w, err = s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	assert.True(t, w.Charging)
	assert.Zero(t, w.Credit)

	// Usage au-delà de l'offre gratuite : 3 000 FCFA par heure pendant deux heures.
	now = activatedAt.Add(3 * time.Hour)
	h1, h2 := now.Truncate(time.Hour).Add(-2*time.Hour), now.Truncate(time.Hour).Add(-time.Hour)
	for _, h := range []time.Time{h1, h2} {
		require.NoError(t, repository.New(db).RecordUsage(ctx, []domain.Usage{{Hour: h, OrganizationID: org, ProjectID: project,
			Meter: domain.MeterCompute, ResourceID: uuid.New(), ResourceName: "big", Size: "custom", Quantity: 1,
			UnitPrice: domain.FromUnits(3000), Amount: domain.FromUnits(3000), Currency: "XAF", CatalogVersion: "test"}}))
	}
	n, err := s.Charge(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)
	w, err = s.Wallet(ctx, org)
	require.NoError(t, err)
	// 6 000 d'usage, moins le Mini offert (1 500) = 4 500, pris sur l'argent : solde négatif, grâce.
	assert.Equal(t, int64(-4500), w.Cash)
	assert.Equal(t, domain.WalletEmpty, w.Status)
	require.NotNil(t, w.GraceUntil, "la grâce commence")
	_, err = s.Charge(ctx)
	require.NoError(t, err)
	w2, _ := s.Wallet(ctx, org)
	assert.Equal(t, w.Balance, w2.Balance, "relancer le débit ne débite rien de plus")

	// Recharge : en attente, puis réussie par le webhook, créditée une seule fois.
	v, err := s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 10000, Phone: "+242 06 123 45 67", Operator: "cg_mtn"}, "e2e")
	require.NoError(t, err)
	assert.Equal(t, domain.TopupPending, v.Status)
	assert.Equal(t, "+242 •••• 4567", v.Phone)
	_, err = s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 10000, Phone: "+242061234567", Operator: "cg_mtn"}, "e2e")
	assert.ErrorIs(t, err, domain.ErrTopupPending, "une seule demande à la fois")

	pay.status = "succeeded"
	require.NoError(t, signed(t, s, "pay-"+v.ID, now))
	require.NoError(t, signed(t, s, "pay-"+v.ID, now))
	got, err := s.Topup(ctx, org, uuid.MustParse(v.ID))
	require.NoError(t, err)
	assert.Equal(t, domain.TopupSucceeded, got.Status)
	w, _ = s.Wallet(ctx, org)
	assert.Equal(t, int64(5500), w.Cash, "crédité une fois (10 000 − 4 500)")
	assert.Equal(t, int64(5000), w.Credit, "crédit d'essai à la première recharge")
	assert.Nil(t, w.GraceUntil, "solde positif : fin de la grâce")
	assert.ErrorIs(t, s.HandleWebhook(ctx, "t=1,v1=bad", []byte(`{}`)), domain.ErrBadWebhook)

	_, err = s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 10, Phone: "+242061234567", Operator: "cg_mtn"}, "e2e")
	assert.ErrorIs(t, err, domain.ErrTopupAmount)

	// Geste de l'équipe, puis expiration du crédit d'essai.
	w, err = s.Adjust(ctx, org, domain.AdjustmentRequest{Amount: -2500, Reason: "correction"}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, int64(3000), w.Cash)
	now = now.Add(31 * 24 * time.Hour)
	expired, err := repository.NewLedger(db).ExpireCredits(ctx, now)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, expired, 1)
	cash, credit, err := repository.NewLedger(db).Balances(ctx, org)
	require.NoError(t, err)
	assert.Zero(t, credit)
	assert.Equal(t, domain.FromUnits(3000), cash)

	// Historique : les débits d'usage du jour réunis en une ligne.
	hist, err := s.Transactions(ctx, org, "", 50)
	require.NoError(t, err)
	kinds := map[string]int{}
	for _, tx := range hist.Transactions {
		kinds[tx.Kind]++
	}
	assert.Equal(t, 1, kinds[domain.TxUsage])
	assert.Equal(t, 1, kinds[domain.TxTopup])
	assert.Equal(t, 1, kinds[domain.TxCreditGrant])
	assert.Equal(t, 1, kinds[domain.TxCreditExpiry])

	// Administration : l'organisation se retrouve et sa fiche se lit.
	list, err := s.Orgs(ctx, "e2e", 200)
	require.NoError(t, err)
	found := false
	for _, o := range list.Organizations {
		if o.ID == org.String() {
			found = true
			assert.True(t, o.Charging)
			assert.Equal(t, int64(3000), o.Balance)
		}
	}
	assert.True(t, found)
	detail, err := s.Org(ctx, org)
	require.NoError(t, err)
	assert.Len(t, detail.Projects, 1)
	rev, err := s.Revenue(ctx, 40)
	require.NoError(t, err)
	assert.NotEmpty(t, rev.Days)
}
