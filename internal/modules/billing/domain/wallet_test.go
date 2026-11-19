package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocate_CreditsFirstSoonestExpiryFirst(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	soon, later, gone := uuid.New(), uuid.New(), uuid.New()
	lots := []CreditLot{
		{ID: gone, Remaining: FromUnits(1000), ExpiresAt: now.Add(-time.Hour)}, // expiré : ignoré
		{ID: soon, Remaining: FromUnits(300), ExpiresAt: now.Add(24 * time.Hour)},
		{ID: later, Remaining: FromUnits(500), ExpiresAt: now.Add(48 * time.Hour)},
	}
	a := Allocate(FromUnits(1000), lots, now)
	assert.Equal(t, FromUnits(300), a.FromCredits[soon])
	assert.Equal(t, FromUnits(500), a.FromCredits[later])
	assert.Zero(t, a.FromCredits[gone])
	assert.Equal(t, FromUnits(800), a.Credit)
	assert.Equal(t, FromUnits(200), a.Cash, "le reste sur l'argent")

	small := Allocate(FromUnits(100), lots, now)
	assert.Equal(t, FromUnits(100), small.Credit)
	assert.Zero(t, small.Cash)
}

func TestNewTransaction(t *testing.T) {
	org := uuid.New()
	now := time.Now()
	a := Allocate(FromUnits(70), []CreditLot{{ID: uuid.New(), Remaining: FromUnits(50), ExpiresAt: now.Add(time.Hour)}}, now)
	tx, err := NewTransaction(org, TxUsage, "ref", "XAF", "", "billing", UsageEntries(org, a), now)
	require.NoError(t, err)
	var sum Milli
	for _, e := range tx.Entries {
		sum += e.Amount
	}
	assert.Zero(t, sum)
	assert.Len(t, tx.Entries, 4)

	_, err = NewTransaction(org, TxAdjustment, "ref2", "XAF", "", "x", map[string]Milli{CashAccount(org): 10, AccountAdjustments: -9}, now)
	assert.ErrorIs(t, err, ErrUnbalanced)
	_, err = NewTransaction(org, TxAdjustment, "ref3", "XAF", "", "x", map[string]Milli{CashAccount(org): 0}, now)
	assert.ErrorIs(t, err, ErrZeroAmount)
}

func TestWalletStatus(t *testing.T) {
	day := FromUnits(100)
	assert.Equal(t, WalletEstimate, WalletStatus(false, 0, day))
	assert.Equal(t, WalletEmpty, WalletStatus(true, 0, day))
	assert.Equal(t, WalletEmpty, WalletStatus(true, -5, day))
	assert.Equal(t, WalletCritical, WalletStatus(true, FromUnits(150), day))
	assert.Equal(t, WalletLow, WalletStatus(true, FromUnits(500), day))
	assert.Equal(t, WalletOK, WalletStatus(true, FromUnits(800), day))
	assert.Equal(t, WalletOK, WalletStatus(true, FromUnits(1), 0), "sans consommation")
	assert.Greater(t, AlertLevel(WalletEmpty), AlertLevel(WalletCritical))
	assert.Equal(t, int64(3), *DaysLeft(FromUnits(350), day))
	assert.Nil(t, DaysLeft(FromUnits(350), 0))
}

func TestNormalizePhone(t *testing.T) {
	assert.Equal(t, "+242061234567", NormalizePhone("+242 06 123 45 67"))
	assert.Equal(t, "+242061234567", NormalizePhone("00242-06-123-45-67"))
	assert.Empty(t, NormalizePhone("06 123 45 67"), "indicatif exigé")
	assert.Empty(t, NormalizePhone("+24206abc"))
	assert.True(t, ValidOperator("cg_mtn"))
	assert.False(t, ValidOperator("MTN"))
	assert.Equal(t, "+242 •••• 4567", MaskPhone("+242061234567"))
}

func TestCatalogTopupBounds(t *testing.T) {
	u := testUpdate()
	u.TopupMin, u.TopupMax, u.TrialCredit, u.TrialDays = 500, 500000, 5000, 30
	c, err := CatalogFromUpdate(u)
	require.NoError(t, err)
	assert.Equal(t, FromUnits(500), c.TopupMin)
	assert.Equal(t, int64(500000), c.Update().TopupMax)

	u.TopupMin = 600000
	_, err = CatalogFromUpdate(u)
	assert.ErrorIs(t, err, ErrInvalidCatalog)
	u.TopupMin, u.TrialDays = 500, 0
	_, err = CatalogFromUpdate(u)
	assert.ErrorIs(t, err, ErrInvalidCatalog, "crédit d'essai sans durée")
}

func TestNetCost_MatchesTheEstimate(t *testing.T) {
	c := fcfaCatalog(t)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	small := PerHour(FromUnits(3000))
	totals := []Total{
		{Meter: MeterCompute, ResourceID: uuid.New(), Hours: 100, Sum: small * 100, Peak: small},
		{Meter: MeterBuildMinutes, ResourceID: uuid.New(), Sum: FromUnits(2000), Peak: FromUnits(2000)},
	}
	e := c.BuildEstimate(month, totals, nil, nil, 0, month)
	assert.Equal(t, e.Total, c.NetCost(totals).Units())
}
