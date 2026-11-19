package repository_test

// Phase 4 sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/billing/... -count=1 -v

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// fakeContainers : arrêts et redémarrages demandés par la facturation.
type fakeContainers struct{ stopped, started []uuid.UUID }

func (f *fakeContainers) Stop(_ context.Context, id uuid.UUID) error {
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeContainers) Start(_ context.Context, id uuid.UUID) error {
	f.started = append(f.started, id)
	return nil
}

func usageAt(t *testing.T, db *gorm.DB, org, project uuid.UUID, hour time.Time, units int64) {
	t.Helper()
	require.NoError(t, repository.New(db).RecordUsage(context.Background(), []domain.Usage{{Hour: hour, OrganizationID: org,
		ProjectID: project, Meter: domain.MeterCompute, ResourceID: uuid.New(), ResourceName: "big", Size: "custom", Quantity: 1,
		UnitPrice: domain.FromUnits(units), Amount: domain.FromUnits(units), Currency: "XAF", CatalogVersion: "test"}}))
}

func runningAPI(t *testing.T, db *gorm.DB, project uuid.UUID) uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	require.NoError(t, db.Raw(`SELECT id FROM workloads WHERE project_id = ? AND name = 'api'`, project).Scan(&ids).Error)
	require.Len(t, ids, 1)
	return ids[0]
}

func TestE2E_GraceSuspensionResumeAndBudget(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	// Date fixe, loin de tout changement de mois.
	now := time.Date(2030, 3, 10, 12, 10, 0, 0, time.UTC)
	pay := &fakePayments{status: "pending"}
	s := walletService(db, &now, pay)
	containers := &fakeContainers{}
	s.Containers = containers
	api := runningAPI(t, db, project)

	_, err := s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	now = now.Add(2 * time.Hour)
	usageAt(t, db, org, project, now.Truncate(time.Hour).Add(-time.Hour), 3000)
	_, err = s.Charge(ctx)
	require.NoError(t, err)
	w, err := s.Wallet(ctx, org)
	require.NoError(t, err)
	assert.Equal(t, int64(-1500), w.Balance, "3 000 − l'offre gratuite (1 500)")
	require.NotNil(t, w.GraceUntil)
	assert.NoError(t, s.CanRun(ctx, project), "pendant la grâce, tout tourne")

	// Grâce écoulée (3 jours de la grille) : suspension douce.
	now = now.Add(73 * time.Hour)
	_, err = s.Charge(ctx)
	require.NoError(t, err)
	w, _ = s.Wallet(ctx, org)
	require.NotNil(t, w.SuspendedAt)
	assert.Contains(t, containers.stopped, api, "le conteneur en marche est arrêté")
	assert.Len(t, containers.stopped, 1, "le conteneur déjà arrêté par le client n'est pas compté")
	assert.ErrorIs(t, s.CanRun(ctx, project), domain.ErrAccountSuspended)

	// Recharge réussie : reprise automatique, seuls les conteneurs arrêtés par la suspension redémarrent.
	pay.status = "succeeded"
	v, err := s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 20000, Phone: "+242069999999", Operator: "cg_airtel"}, "e2e")
	require.NoError(t, err)
	require.NoError(t, signed(t, s, "pay-"+v.ID, now))
	w, _ = s.Wallet(ctx, org)
	assert.Nil(t, w.SuspendedAt)
	assert.Nil(t, w.GraceUntil)
	assert.Equal(t, []uuid.UUID{api}, containers.started)
	assert.NoError(t, s.CanRun(ctx, project))
	assert.Equal(t, int64(5000), w.Credit, "crédit d'essai : premier numéro")

	// Même numéro pour une autre organisation : pas de second crédit d'essai.
	db2, org2, _ := e2e(t)
	_ = db2
	s2 := walletService(db, &now, pay)
	_, err = s2.SetCharging(ctx, org2, true, "e2e")
	require.NoError(t, err)
	v2, err := s2.CreateTopup(ctx, org2, domain.TopupRequest{Amount: 1000, Phone: "+242069999999", Operator: "cg_airtel"}, "e2e")
	require.NoError(t, err)
	require.NoError(t, signed(t, s2, "pay-"+v2.ID, now))
	w2, _ := s2.Wallet(ctx, org2)
	assert.Zero(t, w2.Credit, "un crédit d'essai par numéro Mobile Money")

	// Budget avec arrêt : dépassé, les conteneurs du projet s'arrêtent ;
	// retiré, ils redémarrent.
	containers.stopped, containers.started = nil, nil
	b, err := s.SetBudget(ctx, project, domain.BudgetRequest{Limit: 1000, Stop: true}, "e2e")
	require.NoError(t, err)
	assert.True(t, b.Stopped)
	assert.Equal(t, []uuid.UUID{api}, containers.stopped)
	assert.ErrorIs(t, s.CanRun(ctx, project), domain.ErrAccountSuspended)
	b, err = s.SetBudget(ctx, project, domain.BudgetRequest{Limit: 0}, "e2e")
	require.NoError(t, err)
	assert.Zero(t, b.Limit)
	assert.Equal(t, []uuid.UUID{api}, containers.started)
	assert.NoError(t, s.CanRun(ctx, project))
}

func TestE2E_InvoicesAndReceipts(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	now := time.Date(2030, 5, 20, 9, 10, 0, 0, time.UTC)
	pay := &fakePayments{status: "succeeded"}
	s := walletService(db, &now, pay)
	s.Issuer = domain.Issuer{Name: "betorun SAS", Address: "Brazzaville, Congo", TaxID: "M2030", Country: "CG",
		VATRateBP: 1890, InvoicePrefix: "TST" + uuid.NewString()[:4], ReceiptPrefix: "RTS" + uuid.NewString()[:4]}

	_, err := s.SetProfile(ctx, org, domain.ProfileRequest{LegalName: "Bomba SARL", TaxID: "NIU-42", Address: "Pointe-Noire", Country: "cg"})
	require.NoError(t, err)
	_, err = s.SetProfile(ctx, org, domain.ProfileRequest{})
	assert.ErrorIs(t, err, domain.ErrInvalidProfile)

	_, err = s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	v, err := s.CreateTopup(ctx, org, domain.TopupRequest{Amount: 50000, Phone: "+242068888888", Operator: "cg_mtn"}, "e2e")
	require.NoError(t, err)
	require.NoError(t, signed(t, s, "pay-"+v.ID, now))

	now = now.Add(2 * time.Hour)
	usageAt(t, db, org, project, now.Truncate(time.Hour).Add(-time.Hour), 10000)
	_, err = s.Charge(ctx)
	require.NoError(t, err)

	// Mois suivant, une fois le mois débité : facture du mois écoulé et reçu.
	now = time.Date(2030, 6, 1, 3, 0, 0, 0, time.UTC)
	s.IssueDocuments(ctx)
	s.IssueDocuments(ctx) // idempotent
	docs, err := s.Documents(ctx, org)
	require.NoError(t, err)
	assert.True(t, docs.IssuerReady)
	require.Len(t, docs.Documents, 2)
	var invoice, receipt domain.DocumentView
	for _, d := range docs.Documents {
		if d.Kind == domain.DocInvoice {
			invoice = d
		} else {
			receipt = d
		}
	}
	assert.Equal(t, "2030-05", invoice.Period)
	assert.Equal(t, s.Issuer.InvoicePrefix+"-2030-000001", invoice.Number)
	// Les recharges d'autres organisations de la base de test ont aussi leur
	// reçu : seul le format compte ici (séquence sans trou par préfixe et par année).
	assert.Regexp(t, "^"+s.Issuer.ReceiptPrefix+`-20\d\d-\d{6}$`, receipt.Number)
	assert.Equal(t, int64(50000), receipt.Total)
	// 10 000 d'usage − 1 500 offerts ; les 5 000 du crédit d'essai d'abord : 3 500 payés en argent.
	assert.Equal(t, int64(3500), invoice.Total)
	assert.Equal(t, int64(556), invoice.VAT, "TVA incluse : 3 500 × 18,90 / 118,90 = 556,35")

	pdfDoc, err := s.DocumentPDF(ctx, org, uuid.MustParse(invoice.ID))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(pdfDoc.PDF, []byte("%PDF")))
	require.Error(t, db.Exec(`UPDATE billing_documents SET total = 0 WHERE id = ?`, invoice.ID).Error, "document immuable")
}
