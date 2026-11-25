package service

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/country"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// memGeo : table pays → région et adresses de mesure en mémoire.
type memGeo struct {
	countries map[string]domain.CountryRegion
	probes    map[string]string
}

func newMemGeo() *memGeo {
	return &memGeo{countries: map[string]domain.CountryRegion{}, probes: map[string]string{}}
}

func (m *memGeo) CountryRegions(context.Context) ([]domain.CountryRegion, error) {
	out := []domain.CountryRegion{}
	for _, e := range m.countries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Country < out[j].Country })
	return out, nil
}
func (m *memGeo) RegionForCountry(_ context.Context, c string) (string, error) {
	return m.countries[c].Region, nil
}
func (m *memGeo) SetCountryRegion(_ context.Context, e *domain.CountryRegion) error {
	m.countries[e.Country] = *e
	return nil
}
func (m *memGeo) DeleteCountryRegion(_ context.Context, c string) error {
	delete(m.countries, c)
	return nil
}
func (m *memGeo) RegionProbes(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.probes {
		if v != "" {
			out[k] = v
		}
	}
	return out, nil
}
func (m *memGeo) SetRegionProbe(_ context.Context, e *domain.RegionSetting) error {
	m.probes[e.Code] = e.ProbeURL
	return nil
}

// geoRegistry : défaut en eu-west-gra, un cluster ouvert en af-central, un
// fermé en af-west (région connue, pas proposée).
func geoRegistry(t *testing.T) (*Registry, *memGeo) {
	t.Helper()
	ctx := context.Background()
	store := newMemRegistry()
	r := newRegistry(store, nil)
	geo := newMemGeo()
	r.Geo = geo
	require.NoError(t, r.EnsureDefault(ctx))
	store.cs["terrain"].Region = "eu-west-gra"
	require.NoError(t, store.Declare(ctx, &domain.Cluster{ClusterID: "lbv-1", Region: "af-central", State: domain.StateActive}))
	require.NoError(t, store.Declare(ctx, &domain.Cluster{ClusterID: "dkr-1", Region: "af-west", State: domain.StateClosed}))
	return r, geo
}

func codes(rs []domain.Region) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Code
	}
	return out
}

func TestRegions_Suggestion(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)
	_, err := r.SetCountryRegion(ctx, "ga", "af-central", "user:staff")
	require.NoError(t, err)
	_, err = r.SetRegionProbe(ctx, "af-central", "https://lbv.example.test/healthz", "user:staff")
	require.NoError(t, err)

	// Pays associé : sa région est suggérée et passe en tête.
	rs, err := r.Regions(ctx, "GA")
	require.NoError(t, err)
	assert.Equal(t, []string{"af-central", "eu-west-gra"}, codes(rs))
	assert.True(t, rs[0].Suggested)
	assert.False(t, rs[1].Suggested)
	assert.True(t, rs[1].Default, "le défaut ne change pas")
	assert.Equal(t, "https://lbv.example.test/healthz", rs[0].ProbeURL)
	assert.Empty(t, rs[1].ProbeURL)

	// Pays sans association, ou inconnu : rien de suggéré, ordre inchangé.
	for _, cc := range []string{"FR", ""} {
		rs, err = r.Regions(ctx, cc)
		require.NoError(t, err)
		assert.Equal(t, []string{"eu-west-gra", "af-central"}, codes(rs), cc)
		for _, x := range rs {
			assert.False(t, x.Suggested, cc)
		}
		assert.Equal(t, "https://lbv.example.test/healthz", rs[1].ProbeURL, "adresse de mesure rendue sans pays")
	}

	// Région associée mais plus proposée (cluster fermé) : rien de suggéré.
	_, err = r.SetCountryRegion(ctx, "SN", "af-west", "")
	require.NoError(t, err)
	rs, err = r.Regions(ctx, "SN")
	require.NoError(t, err)
	assert.Equal(t, []string{"eu-west-gra", "af-central"}, codes(rs))
}

func TestCountryRegions_Admin(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)

	e, err := r.SetCountryRegion(ctx, " cg ", "af-central", "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.CountryRegion{Country: "CG", Region: "af-central", UpdatedBy: "user:staff"}, *e)
	_, err = r.SetCountryRegion(ctx, "GA", "af-west", "")
	require.NoError(t, err, "région d'un cluster fermé : connue, acceptée")

	_, err = r.SetCountryRegion(ctx, "GA", "ap-south", "")
	assert.ErrorIs(t, err, domain.ErrUnknownRegion, "région d'aucun cluster")
	_, err = r.SetCountryRegion(ctx, "GA", "AF CENTRAL", "")
	assert.ErrorIs(t, err, domain.ErrInvalidRegionCode)
	for _, bad := range []string{"XX", "", "GAB"} {
		_, err = r.SetCountryRegion(ctx, bad, "af-central", "")
		assert.ErrorIs(t, err, country.ErrInvalid, bad)
	}

	list, err := r.CountryRegions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "CG", list[0].Country)
	assert.Equal(t, "GA", list[1].Country)

	require.NoError(t, r.DeleteCountryRegion(ctx, "ga"))
	require.NoError(t, r.DeleteCountryRegion(ctx, "GA"), "absente : rien")
	assert.ErrorIs(t, r.DeleteCountryRegion(ctx, "XX"), country.ErrInvalid)
	list, err = r.CountryRegions(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestRegionSettings_Admin(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)

	_, err := r.SetRegionProbe(ctx, "af-central", "http://lbv.example.test/healthz", "")
	assert.ErrorIs(t, err, domain.ErrInvalidProbeURL, "https obligatoire")
	_, err = r.SetRegionProbe(ctx, "ap-south", "https://x.example.test", "")
	assert.ErrorIs(t, err, domain.ErrUnknownRegion)
	s, err := r.SetRegionProbe(ctx, "af-west", " https://dkr.example.test/healthz ", "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "https://dkr.example.test/healthz", s.ProbeURL)

	list, err := r.RegionSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.RegionSetting{{Code: "af-central"}, {Code: "af-west", ProbeURL: "https://dkr.example.test/healthz"}, {Code: "eu-west-gra"}}, list,
		"toutes les régions des clusters enregistrés, triées")

	_, err = r.SetRegionProbe(ctx, "af-west", "", "")
	require.NoError(t, err, "vide : retirée")
	list, err = r.RegionSettings(ctx)
	require.NoError(t, err)
	assert.Empty(t, list[1].ProbeURL)
}

// Sans table (déploiement sans base) : régions sans suggestion, back-office indisponible.
func TestRegions_WithoutGeo(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)
	r.Geo = nil
	rs, err := r.Regions(ctx, "GA")
	require.NoError(t, err)
	assert.Len(t, rs, 2)
	_, err = r.CountryRegions(ctx)
	assert.ErrorIs(t, err, ErrNoRegionStore)
}
