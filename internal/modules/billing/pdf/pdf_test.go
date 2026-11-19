package pdf

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// PDF_OUT=/chemin/facture.pdf : écrit l'exemple pour le regarder.
func TestRender(t *testing.T) {
	issuer := domain.Issuer{Name: "betorun SAS", Address: "12 avenue de l'Indépendance, Brazzaville", TaxID: "M2026000042", Country: "CG", VATRateBP: 1890}
	data := domain.DocData{Issuer: issuer, OrgName: "bomba",
		Customer: domain.Profile{LegalName: "Bomba SARL", TaxID: "NIU-42", Address: "Pointe-Noire", Country: "CG"},
		Lines: []domain.DocLine{
			{Label: "Conteneurs", Amount: domain.FromUnits(18000)},
			{Label: "Bases de données", Amount: domain.FromUnits(3500)},
			{Label: "Offre gratuite du mois", Amount: -domain.FromUnits(3800)},
		},
		PaidByCredits: domain.FromUnits(5000), PaidByCash: domain.FromUnits(12700)}
	cash := domain.FromUnits(12700)
	out, err := Render(Doc{Kind: domain.DocInvoice, Number: "BTR-2026-000042", Period: "2026-11", Currency: "XAF",
		IssuedAt: time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC), Data: data, Total: cash, VAT: domain.SplitVAT(cash, 1890), VATRateBP: 1890})
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(out, []byte("%PDF")))
	if p := os.Getenv("PDF_OUT"); p != "" {
		require.NoError(t, os.WriteFile(p, out, 0o600))
	}

	receipt, err := Render(Doc{Kind: domain.DocReceipt, Number: "REC-2026-000007", Currency: "XAF", IssuedAt: time.Now(),
		Data: domain.DocData{Issuer: issuer, OrgName: "bomba", Payment: "MTN Mobile Money +242 •••• 4567",
			Lines: []domain.DocLine{{Label: "Recharge du solde prépayé", Amount: domain.FromUnits(20000)}}}, Total: domain.FromUnits(20000)})
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(receipt, []byte("%PDF")))
}

func TestSplitVAT(t *testing.T) {
	assert.Equal(t, domain.Milli(556350), domain.SplitVAT(domain.FromUnits(3500), 1890))
	assert.Zero(t, domain.SplitVAT(domain.FromUnits(3500), 0))
}
