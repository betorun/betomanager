package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanFromUpdate(t *testing.T) {
	p, err := PlanFromUpdate(PlanUpdate{ID: "starter", Name: "Starter", Public: true, CPULimit: "4000m", MemoryLimit: "8Gi",
		StorageLimit: "40Gi", Fees: map[string]int64{"xaf": 5000}})
	require.NoError(t, err)
	assert.Equal(t, FromUnits(5000), p.Fee("XAF"))
	assert.Zero(t, p.Fee("XOF"))
	cpu, mem, err := p.Limits()
	require.NoError(t, err)
	assert.Equal(t, int64(4000), cpu)
	assert.Equal(t, int64(8192), mem)

	for name, u := range map[string]PlanUpdate{
		"identifiant": {ID: "Pro!", Name: "x", CPULimit: "1", MemoryLimit: "1Gi", StorageLimit: "1Gi"},
		"nom":         {ID: "pro", CPULimit: "1", MemoryLimit: "1Gi", StorageLimit: "1Gi"},
		"limite":      {ID: "pro", Name: "Pro", CPULimit: "beaucoup", MemoryLimit: "1Gi", StorageLimit: "1Gi"},
		"nulle":       {ID: "pro", Name: "Pro", CPULimit: "0", MemoryLimit: "1Gi", StorageLimit: "1Gi"},
		"prix":        {ID: "pro", Name: "Pro", CPULimit: "1", MemoryLimit: "1Gi", StorageLimit: "1Gi", Fees: map[string]int64{"XAF": -1}},
	} {
		_, err := PlanFromUpdate(u)
		assert.ErrorIs(t, err, ErrInvalidPlan, name)
	}
}

func TestPlanMeteredLikeAResource(t *testing.T) {
	c := fcfaCatalog(t)
	hour := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	org := uuid.New()
	lines := c.Price(Reservation{Kind: KindPlan, OrganizationID: org, ResourceID: org, Size: "pro", Monthly: FromUnits(15000)}, hour)
	require.Len(t, lines, 1)
	assert.Equal(t, MeterPlan, lines[0].Meter)
	assert.Equal(t, PerHour(FromUnits(15000)), lines[0].Amount)
	assert.Empty(t, c.Price(Reservation{Kind: KindPlan, Monthly: 0}, hour), "plan gratuit : rien à mesurer")

	// Plafonné au mois, hors offre gratuite, sans projet.
	rate := PerHour(FromUnits(15000))
	totals := []Total{{Meter: MeterPlan, ResourceID: org, Hours: 744, Sum: rate * 744, Peak: rate}}
	assert.Equal(t, int64(15000), c.NetCost(totals).Units(), "730 h au plus, et l'offre gratuite ne couvre pas l'abonnement")
	e := c.BuildEstimate(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), totals, nil, nil, 0, hour)
	assert.Empty(t, e.Projects, "l'abonnement n'appartient à aucun projet")
	assert.Equal(t, int64(15000), e.Total)
	assert.Equal(t, "Abonnement", c.InvoiceLines(totals)[0].Label)
}
