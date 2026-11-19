package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReceivables_OldestPaidFirst(t *testing.T) {
	now := time.Date(2030, 7, 20, 0, 0, 0, 0, time.UTC)
	due := func(d time.Time) *time.Time { return &d }
	invoices := []Invoice{
		{ID: uuid.New(), Number: "A", Period: "2030-05", Total: FromUnits(1000), IssuedAt: time.Date(2030, 6, 1, 3, 0, 0, 0, time.UTC), DueAt: due(time.Date(2030, 7, 1, 3, 0, 0, 0, time.UTC))},
		{ID: uuid.New(), Number: "B", Period: "2030-06", Total: FromUnits(2000), IssuedAt: time.Date(2030, 7, 1, 3, 0, 0, 0, time.UTC), DueAt: due(time.Date(2030, 7, 31, 3, 0, 0, 0, time.UTC))},
	}
	// 2 500 dus : B entier (2 000), A pour 500 ; A est en retard de 18 jours.
	r := Receivables(invoices, FromUnits(2500), now)
	require.Len(t, r, 2)
	assert.Equal(t, "A", r[0].Number)
	assert.Equal(t, int64(500), r[0].Unpaid)
	assert.Equal(t, 18, r[0].DaysOverdue)
	assert.Equal(t, int64(2000), r[1].Unpaid)
	assert.Zero(t, r[1].DaysOverdue)

	assert.Len(t, Receivables(invoices, FromUnits(1500), now), 1, "A est soldée")
	assert.Empty(t, Receivables(invoices, 0, now))
}

func TestInvoicedDebt(t *testing.T) {
	assert.Zero(t, InvoicedDebt(FromUnits(100), 0), "solde positif : rien dû")
	assert.Equal(t, FromUnits(700), InvoicedDebt(-FromUnits(1000), FromUnits(300)), "300 de ce mois pas encore facturés")
	assert.Zero(t, InvoicedDebt(-FromUnits(200), FromUnits(300)))
}
