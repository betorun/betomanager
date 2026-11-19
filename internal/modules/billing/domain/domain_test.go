package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testUpdate : grille de test (mêmes montants que la grille de départ amorcée
// par la migration 000034).
func testUpdate() CatalogUpdate {
	return CatalogUpdate{
		Currency: "xaf",
		Sizes: []SizeView{
			{ID: "mini", CPUMilli: 250, MemMi: 256, Monthly: 1500},
			{ID: "small", CPUMilli: 500, MemMi: 512, Monthly: 3000},
			{ID: "medium", CPUMilli: 1000, MemMi: 1024, Monthly: 6000},
			{ID: "large", CPUMilli: 2000, MemMi: 2048, Monthly: 12000},
			{ID: "xlarge", CPUMilli: 4000, MemMi: 4096, Monthly: 24000},
		},
		DatabaseMonthly: 2000, DiskGiMonthly: 300, BuildMinute: 10,
		Free: FreeView{Compute: 1500, Database: 2300, BuildMinutes: 100},
	}
}

func fcfaCatalog(t *testing.T) Catalog {
	c, err := CatalogFromUpdate(testUpdate())
	require.NoError(t, err)
	return c
}

func TestUnits_RoundsHalfUp(t *testing.T) {
	assert.Equal(t, int64(2), Milli(1500).Units())
	assert.Equal(t, int64(1), Milli(1499).Units())
	assert.Equal(t, int64(-2), Milli(-1500).Units())
}

func TestCatalogFromUpdate(t *testing.T) {
	c := fcfaCatalog(t)
	assert.Equal(t, "XAF", c.Currency)
	assert.Equal(t, FromUnits(3000), c.Sizes[1].Monthly)
	assert.Equal(t, testUpdate().Sizes, c.Update().Sizes, "aller-retour sans perte")

	bad := map[string]func(*CatalogUpdate){
		"monnaie":                      func(u *CatalogUpdate) { u.Currency = "francs" },
		"aucune taille":                func(u *CatalogUpdate) { u.Sizes = nil },
		"taille réservée":              func(u *CatalogUpdate) { u.Sizes[0].ID = SizeCustom },
		"taille en double":             func(u *CatalogUpdate) { u.Sizes[1].ID = "mini" },
		"prix nul":                     func(u *CatalogUpdate) { u.Sizes[0].Monthly = 0 },
		"tailles en désordre":          func(u *CatalogUpdate) { u.Sizes[0], u.Sizes[1] = u.Sizes[1], u.Sizes[0] },
		"plus grande mais moins chère": func(u *CatalogUpdate) { u.Sizes[1].Monthly = 1000 },
		"montant négatif":              func(u *CatalogUpdate) { u.DiskGiMonthly = -1 },
	}
	for name, mutate := range bad {
		u := testUpdate()
		u.Sizes = append([]SizeView(nil), u.Sizes...)
		mutate(&u)
		_, err := CatalogFromUpdate(u)
		assert.ErrorIs(t, err, ErrInvalidCatalog, name)
	}
}

func TestSizeFor(t *testing.T) {
	c := fcfaCatalog(t)
	cases := []struct {
		cpu, mem int64
		size     string
		monthly  int64
	}{
		{200, 256, "mini", 1500},
		{250, 300, "small", 3000}, // la mémoire fait monter d'une taille
		{1000, 512, "medium", 6000},
		{4000, 4096, "xlarge", 24000},
		{8000, 4096, SizeCustom, 48000}, // deux fois le CPU du plus grand
	}
	for _, tc := range cases {
		size, monthly := c.SizeFor(tc.cpu, tc.mem)
		assert.Equal(t, tc.size, size, "%d/%d", tc.cpu, tc.mem)
		assert.Equal(t, tc.monthly, monthly.Units(), "%d/%d", tc.cpu, tc.mem)
	}
}

func TestPerHour_NeverBelowTheMonth(t *testing.T) {
	m := FromUnits(1500)
	assert.GreaterOrEqual(t, PerHour(m)*HoursPerMonth, m)
}

func TestPrice(t *testing.T) {
	c := fcfaCatalog(t)
	hour := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

	running := c.Price(Reservation{Kind: KindContainer, CPUMilli: 500, MemMi: 512, Replicas: 2, DiskGi: 1}, hour)
	require.Len(t, running, 2)
	assert.Equal(t, MeterCompute, running[0].Meter)
	assert.Equal(t, "small", running[0].Size)
	assert.Equal(t, PerHour(FromUnits(3000))*2, running[0].Amount)
	assert.Equal(t, MeterDisk, running[1].Meter)

	stopped := c.Price(Reservation{Kind: KindContainer, CPUMilli: 500, MemMi: 512, Replicas: 0, DiskGi: 2}, hour)
	require.Len(t, stopped, 1, "arrêté : seul le disque compte")
	assert.Equal(t, MeterDisk, stopped[0].Meter)

	db := c.Price(Reservation{Kind: KindDatabase, DiskGi: 5}, hour)
	require.Len(t, db, 2)
	assert.Equal(t, int64(5), db[1].Quantity)

	finished := hour.Add(-90 * time.Minute)
	build := c.Price(Reservation{Kind: KindBuild, BuildMinutes: 7, FinishedAt: finished}, hour)
	require.Len(t, build, 1)
	assert.Equal(t, finished.Truncate(time.Hour), build[0].Hour, "compté dans l'heure de fin")
	assert.Equal(t, FromUnits(70), build[0].Amount)

	assert.Equal(t, Milli(0), c.Price(Reservation{Kind: KindSiteDomain}, hour)[0].Amount, "gratuit au début, mais mesuré")
}

func TestTotalCost_CappedAtTheMonth(t *testing.T) {
	rate := PerHour(FromUnits(1500))
	over := Total{Meter: MeterCompute, Sum: rate * 744, Peak: rate}
	assert.Equal(t, rate*HoursPerMonth, over.Cost())
	builds := Total{Meter: MeterBuildMinutes, Sum: FromUnits(5000), Peak: FromUnits(100)}
	assert.Equal(t, FromUnits(5000), builds.Cost(), "les minutes ne sont pas plafonnées")
}

func TestBuildEstimate_FreeTier(t *testing.T) {
	c := fcfaCatalog(t)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := uuid.New()
	mini, small := PerHour(FromUnits(1500)), PerHour(FromUnits(3000))
	totals := []Total{
		{ProjectID: p, Meter: MeterCompute, ResourceID: uuid.New(), Size: "mini", Quantity: 1, Hours: 730, Sum: mini * 730, Peak: mini},
		{ProjectID: p, Meter: MeterCompute, ResourceID: uuid.New(), Size: "small", Quantity: 1, Hours: 730, Sum: small * 730, Peak: small},
		{ProjectID: p, Meter: MeterBuildMinutes, ResourceID: uuid.New(), Quantity: 150, Sum: FromUnits(1500), Peak: FromUnits(1500)},
	}
	e := c.BuildEstimate(month, totals, map[uuid.UUID]string{p: "bomba"}, nil, 0, month)

	assert.Equal(t, "2026-11", e.Month)
	assert.Equal(t, int64(1500), e.FreeTier.Compute, "un Mini offert")
	assert.Equal(t, int64(1000), e.FreeTier.BuildMinutes, "100 minutes offertes")
	assert.Equal(t, int64(0), e.FreeTier.Database)
	assert.Equal(t, e.Gross-e.FreeTier.Total, e.Total)
	require.Len(t, e.Projects, 1)
	assert.Equal(t, "bomba", e.Projects[0].Name)
}

func TestBuildEstimate_Projection(t *testing.T) {
	c := fcfaCatalog(t)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := month.Add(10 * time.Hour)
	id := uuid.New()
	small := PerHour(FromUnits(3000))
	totals := []Total{{ProjectID: uuid.New(), Meter: MeterCompute, ResourceID: id, Quantity: 1, Hours: 11, Sum: small * 11, Peak: small}} // 0 h à 10 h comprise : l'heure en cours est déjà relevée
	last := map[uuid.UUID]map[Meter]Milli{id: {MeterCompute: small}}
	e := c.BuildEstimate(month, totals, nil, last, HoursLeft(month, now), now)
	// 720 h dans novembre : le petit conteneur plein mois, moins le Mini offert.
	assert.InDelta(t, 3000*720/730-1500, e.Projected, 2)
}

func TestHoursLeft(t *testing.T) {
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, int64(719), HoursLeft(month, month))
	assert.Equal(t, int64(0), HoursLeft(month, month.AddDate(0, 1, 0)))
}

func TestParseMonth(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	m, err := ParseMonth("", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), m)
	_, err = ParseMonth("novembre", now)
	assert.ErrorIs(t, err, ErrInvalidMonth)
}
