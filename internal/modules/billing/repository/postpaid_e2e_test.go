package repository_test

// Postpayé sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/billing/... -count=1 -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

func TestE2E_Postpaid(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	now := time.Date(2030, 9, 12, 12, 10, 0, 0, time.UTC)
	s := walletService(db, &now, &fakePayments{status: "pending"})
	s.Postpaid = repository.NewPostpaid(db)
	containers := &fakeContainers{}
	s.Containers = containers
	s.Issuer = domain.Issuer{Name: "betorun SAS", Address: "Brazzaville", VATRateBP: 1890, PaymentInstructions: "RIB : CG00 0000",
		InvoicePrefix: "PP" + uuid.NewString()[:4], ReceiptPrefix: "PR" + uuid.NewString()[:4]}
	api := runningAPI(t, db, project)

	// Postpayé : la facturation réelle s'active, plafond de la grille (200 000).
	w, err := s.SetPostpaid(ctx, org, domain.PostpaidRequest{Enabled: true}, "user:staff")
	require.NoError(t, err)
	assert.True(t, w.Charging)
	require.NotNil(t, w.Postpaid)
	assert.Equal(t, int64(200000), w.Postpaid.CreditLimit)
	assert.Equal(t, 30, w.Postpaid.TermsDays)

	// Usage : une dette, sous le plafond : ni grâce ni alerte.
	now = now.Add(2 * time.Hour)
	usageAt(t, db, org, project, now.Truncate(time.Hour).Add(-time.Hour), 3000)
	_, err = s.Charge(ctx)
	require.NoError(t, err)
	w, _ = s.Wallet(ctx, org)
	assert.Equal(t, int64(-1500), w.Balance)
	assert.Equal(t, int64(1500), w.Postpaid.Outstanding)
	assert.Equal(t, int64(198500), w.Postpaid.Available)
	assert.Nil(t, w.GraceUntil, "dans le plafond")
	assert.Empty(t, w.Postpaid.Invoices, "pas encore facturé")

	// Facture du mois, à payer sous 30 jours.
	now = time.Date(2030, 10, 1, 3, 0, 0, 0, time.UTC)
	s.IssueDocuments(ctx)
	w, _ = s.Wallet(ctx, org)
	require.Len(t, w.Postpaid.Invoices, 1)
	inv := w.Postpaid.Invoices[0]
	assert.Equal(t, int64(1500), inv.Unpaid)
	require.NotNil(t, inv.DueAt)
	assert.Equal(t, now.Add(30*24*time.Hour), inv.DueAt.UTC())

	// En retard de 16 jours (suspension à 15) : conteneurs arrêtés.
	now = inv.DueAt.Add(16 * 24 * time.Hour)
	s.RunPostpaid(ctx)
	w, _ = s.Wallet(ctx, org)
	require.NotNil(t, w.SuspendedAt)
	assert.Contains(t, containers.stopped, api)
	assert.ErrorIs(t, s.CanRun(ctx, project), domain.ErrAccountSuspended)

	// Virement reçu : facture soldée, reprise.
	w, err = s.RecordPayment(ctx, org, domain.PaymentRequest{Amount: 1500, Reference: "VIR 2030-11 Bomba"}, "user:staff")
	require.NoError(t, err)
	assert.Empty(t, w.Postpaid.Invoices)
	assert.Nil(t, w.SuspendedAt)
	assert.Contains(t, containers.started, api)
	_, err = s.RecordPayment(ctx, org, domain.PaymentRequest{Amount: 0, Reference: "x"}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrInvalidPayment)

	// Retour au prépayé.
	w, err = s.SetPostpaid(ctx, org, domain.PostpaidRequest{Enabled: false}, "user:staff")
	require.NoError(t, err)
	assert.Nil(t, w.Postpaid)
}
