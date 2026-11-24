package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

type key struct {
	meter domain.Meter
	id    uuid.UUID
	hour  time.Time
}

// memStore : même règle que la base (le plus haut montant de l'heure gagne).
type memStore struct {
	rows     map[key]domain.Usage
	currency string
	catalogs map[string][]domain.Catalog // versions, la dernière en vigueur
}

func (m *memStore) ActiveCatalog(_ context.Context, currency string) (domain.Catalog, error) {
	v := m.catalogs[currency]
	if len(v) == 0 {
		return domain.Catalog{}, domain.ErrUnknownCurrency
	}
	return v[len(v)-1], nil
}

func (m *memStore) ActiveCatalogs(context.Context) ([]domain.CatalogVersionView, error) {
	var out []domain.CatalogVersionView
	for _, v := range m.catalogs {
		out = append(out, domain.CatalogVersionView{CatalogView: v[len(v)-1].View()})
	}
	return out, nil
}

func (m *memStore) SaveCatalog(_ context.Context, c domain.Catalog, _ string) (domain.Catalog, error) {
	c.Version = strconv.Itoa(len(m.catalogs[c.Currency]) + 1)
	m.catalogs[c.Currency] = append(m.catalogs[c.Currency], c)
	return c, nil
}

func seedCatalog(t *testing.T, m *memStore) {
	c, err := domain.CatalogFromUpdate(domain.CatalogUpdate{
		Currency: "XAF",
		Sizes: []domain.SizeView{
			{ID: "mini", CPUMilli: 250, MemMi: 256, Monthly: 1500},
			{ID: "small", CPUMilli: 500, MemMi: 512, Monthly: 3000},
			{ID: "large", CPUMilli: 2000, MemMi: 2048, Monthly: 12000},
		},
		DatabaseMonthly: 2000, DiskGiMonthly: 300, BuildMinute: 10,
		Free: domain.FreeView{Compute: 1500, Database: 2300, BuildMinutes: 100},
	})
	require.NoError(t, err)
	_, _ = m.SaveCatalog(context.Background(), c, "test")
}

func (m *memStore) RecordUsage(_ context.Context, lines []domain.Usage) error {
	for _, l := range lines {
		k := key{l.Meter, l.ResourceID, l.Hour}
		if old, ok := m.rows[k]; !ok || l.Amount > old.Amount {
			m.rows[k] = l
		}
	}
	return nil
}

func (m *memStore) Totals(_ context.Context, org uuid.UUID, from, to time.Time) ([]domain.Total, error) {
	acc := map[uuid.UUID]*domain.Total{}
	for k, l := range m.rows {
		if l.OrganizationID != org || k.hour.Before(from) || !k.hour.Before(to) {
			continue
		}
		t := acc[l.ResourceID]
		if t == nil {
			t = &domain.Total{ProjectID: l.ProjectID, Meter: l.Meter, ResourceID: l.ResourceID, ResourceName: l.ResourceName, Size: l.Size, Quantity: l.Quantity}
			acc[l.ResourceID] = t
		}
		t.Hours++
		t.Sum += l.Amount
		t.Peak = max(t.Peak, l.Amount)
	}
	var out []domain.Total
	for _, t := range acc {
		out = append(out, *t)
	}
	return out, nil
}

func (m *memStore) HourAmounts(_ context.Context, org uuid.UUID, hour time.Time) (map[uuid.UUID]map[domain.Meter]domain.Milli, error) {
	out := map[uuid.UUID]map[domain.Meter]domain.Milli{}
	for k, l := range m.rows {
		if l.OrganizationID == org && k.hour.Equal(hour) && k.meter.Hourly() {
			out[k.id] = map[domain.Meter]domain.Milli{k.meter: l.Amount}
		}
	}
	return out, nil
}

func (m *memStore) Currency(context.Context, uuid.UUID) (string, error) {
	if m.currency == "" {
		return domain.DefaultCurrency, nil
	}
	return m.currency, nil
}

type fakeInventory struct {
	res     []domain.Reservation
	orgErr  error // erreur de ProjectOrg (nil : organisation nulle)
	pending bool  // projet en suppression programmée
}

func (f *fakeInventory) ProjectPendingDeletion(context.Context, uuid.UUID) (bool, error) {
	return f.pending, nil
}

func (f *fakeInventory) Reservations(context.Context, time.Time) ([]domain.Reservation, error) {
	return f.res, nil
}
func (f *fakeInventory) RunningWorkloads(context.Context, uuid.UUID, *uuid.UUID) ([]domain.WorkloadRef, error) {
	return nil, nil
}
func (f *fakeInventory) LimitTargets(context.Context, *uuid.UUID, *string) ([]domain.LimitTarget, error) {
	return nil, nil
}
func (f *fakeInventory) ProjectUsages(context.Context, uuid.UUID) ([]domain.ProjectUsage, error) {
	return nil, nil
}
func (f *fakeInventory) ProjectOrg(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, f.orgErr
}
func (f *fakeInventory) ProjectNames(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		out[id] = "bomba"
	}
	return out, nil
}

func setup(t *testing.T, now *time.Time) (*Service, *memStore, *fakeInventory, uuid.UUID) {
	org, project := uuid.New(), uuid.New()
	inv := &fakeInventory{res: []domain.Reservation{
		{Kind: domain.KindContainer, OrganizationID: org, ProjectID: project, ResourceID: uuid.New(), ResourceName: "bomba-api", CPUMilli: 500, MemMi: 512, Replicas: 1},
		{Kind: domain.KindDatabase, OrganizationID: org, ProjectID: project, ResourceID: uuid.New(), ResourceName: "bomba-db", DiskGi: 5},
	}}
	store := &memStore{rows: map[key]domain.Usage{}, catalogs: map[string][]domain.Catalog{}}
	seedCatalog(t, store)
	return &Service{Store: store, Inventory: inv, Now: func() time.Time { return *now }}, store, inv, org
}

func TestSample_IdempotentWithinTheHour(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC)
	s, store, _, _ := setup(t, &now)
	_, err := s.Sample(context.Background())
	require.NoError(t, err)
	now = now.Add(20 * time.Minute)
	_, err = s.Sample(context.Background())
	require.NoError(t, err)
	assert.Len(t, store.rows, 3, "conteneur, base, disque de la base : une ligne chacun pour l'heure")
}

func TestSample_KeepsTheLargestSizeOfTheHour(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC)
	s, store, inv, _ := setup(t, &now)
	_, _ = s.Sample(context.Background())
	inv.res[0].CPUMilli = 2000 // agrandi en cours d'heure
	now = now.Add(10 * time.Minute)
	_, _ = s.Sample(context.Background())
	inv.res[0].CPUMilli = 200 // puis réduit
	now = now.Add(10 * time.Minute)
	_, _ = s.Sample(context.Background())
	k := key{domain.MeterCompute, inv.res[0].ResourceID, now.Truncate(time.Hour)}
	assert.Equal(t, "large", store.rows[k].Size)
}

func TestEstimate(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	s, _, _, org := setup(t, &now)
	for range 3 { // trois heures relevées
		_, err := s.Sample(context.Background())
		require.NoError(t, err)
		now = now.Add(time.Hour)
	}
	now = now.Add(-time.Hour)
	e, err := s.Estimate(context.Background(), org, "")
	require.NoError(t, err)
	assert.Equal(t, "2026-11", e.Month)
	assert.Equal(t, "XAF", e.Currency)
	require.Len(t, e.Projects, 1)
	assert.Equal(t, "bomba", e.Projects[0].Name)
	assert.Positive(t, e.Gross)
	assert.Equal(t, e.Gross, e.FreeTier.Total, "trois heures restent sous l'offre gratuite")
	assert.Equal(t, int64(0), e.Total)
	assert.Positive(t, e.Projected, "au rythme actuel, le mois dépasse l'offre gratuite")
}

func TestEstimate_InvalidMonth(t *testing.T) {
	now := time.Now()
	s, _, _, org := setup(t, &now)
	_, err := s.Estimate(context.Background(), org, "11-2026")
	assert.ErrorIs(t, err, domain.ErrInvalidMonth)
}

func TestCatalog(t *testing.T) {
	now := time.Now()
	s, _, _, _ := setup(t, &now)
	v, err := s.Catalog(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "XAF", v.Currency)
	assert.Equal(t, int64(3000), v.Sizes[1].Monthly)
	_, err = s.Catalog(context.Background(), "USD")
	assert.ErrorIs(t, err, domain.ErrUnknownCurrency)
}

func TestUpdateCatalog_NewVersionAppliesToTheNextSample(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC)
	s, store, inv, _ := setup(t, &now)
	u := domain.CatalogUpdate{Currency: "XAF", Sizes: []domain.SizeView{{ID: "mini", CPUMilli: 1000, MemMi: 1024, Monthly: 1000}},
		DatabaseMonthly: 2000, DiskGiMonthly: 300, BuildMinute: 10}
	v, err := s.UpdateCatalog(context.Background(), u, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "2", v.Version)

	_, err = s.Sample(context.Background())
	require.NoError(t, err)
	line := store.rows[key{domain.MeterCompute, inv.res[0].ResourceID, now.Truncate(time.Hour)}]
	assert.Equal(t, "mini", line.Size)
	assert.Equal(t, "2", line.CatalogVersion)

	_, err = s.UpdateCatalog(context.Background(), domain.CatalogUpdate{Currency: "XAF"}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrInvalidCatalog)
}

func TestSample_UnknownCurrencyFallsBackToDefault(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC)
	s, store, _, _ := setup(t, &now)
	store.currency = "CDF"
	n, err := s.Sample(context.Background())
	require.NoError(t, err)
	assert.Positive(t, n)
}
