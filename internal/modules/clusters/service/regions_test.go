package service

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// Origines des adresses de mesure : régions proposées seulement (comme
// GET /regions), https, sans chemin ni doublon, triées ; aucune sans magasin.
func TestProbeOrigins(t *testing.T) {
	ctx := context.Background()
	r, geo := geoRegistry(t)
	geo.probes["af-central"] = "https://Probe.LBV.example.com/healthz"
	geo.probes["eu-west-gra"] = "https://probe.gra.example.com:8443/ping?x=1"
	o, err := r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://probe.gra.example.com:8443", "https://probe.lbv.example.com"}, o)

	r.Geo = nil
	o, err = r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Empty(t, o)
	assert.NotNil(t, o, "liste vide, jamais null")
}

// Région connue mais pas proposée (cluster fermé, supprimé, région de
// personne) : son adresse n'est pas publiée ; doublon fusionné.
func TestProbeOrigins_OnlyOfferedRegions(t *testing.T) {
	ctx := context.Background()
	r, geo := geoRegistry(t)
	geo.probes["af-central"] = "https://probe.lbv.example.com/healthz"
	geo.probes["eu-west-gra"] = "https://probe.lbv.example.com/autre"
	geo.probes["af-west"] = "https://probe.dkr.example.com/healthz"
	geo.probes["ap-south"] = "https://probe.ap.example.com/healthz"
	o, err := r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)

	// Cluster supprimé : sa région disparaît au prochain calcul.
	require.NoError(t, r.Delete(ctx, "lbv-1"))
	o, err = r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o, "toujours celle d'eu-west-gra")
	delete(geo.probes, "eu-west-gra")
	r.invalidateProbeOrigins()
	o, err = r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Empty(t, o)
}

// Défense en profondeur : une ligne invalide en base (écrite avant le
// contrôle strict) est ignorée, jamais publiée.
func TestProbeOrigins_SkipsInvalidRows(t *testing.T) {
	ctx := context.Background()
	r, geo := geoRegistry(t)
	geo.probes["af-central"] = "https://a.com;sandbox/x"
	geo.probes["eu-west-gra"] = "http://insecure.example.com/healthz"
	o, err := r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Empty(t, o)

	geo.probes["eu-west-gra"] = "https://*"
	r.invalidateProbeOrigins()
	o, err = r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Empty(t, o)
}

// countingGeo : compte les lectures des adresses de mesure.
type countingGeo struct {
	*memGeo
	reads int
}

func (c *countingGeo) RegionProbes(ctx context.Context) (map[string]string, error) {
	c.reads++
	return c.memGeo.RegionProbes(ctx)
}

// Cache de ProbeOrigins : une lecture par probeOriginsTTL, relue après
// SetRegionProbe ou une écriture du registre.
func TestProbeOrigins_Cache(t *testing.T) {
	ctx := context.Background()
	r, mem := geoRegistry(t)
	geo := &countingGeo{memGeo: mem}
	r.Geo = geo
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	mem.probes["af-central"] = "https://probe.lbv.example.com/healthz"

	for range 3 {
		o, err := r.ProbeOrigins(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)
	}
	assert.Equal(t, 1, geo.reads, "servies par le cache")

	// La copie rendue ne touche pas au cache.
	o, _ := r.ProbeOrigins(ctx)
	o[0] = "https://altered.example.com"
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)

	// Changement direct en base : invisible avant l'expiration.
	mem.probes["af-central"] = "https://probe2.lbv.example.com/healthz"
	now = now.Add(probeOriginsTTL - time.Second)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)
	now = now.Add(time.Second)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe2.lbv.example.com"}, o, "expiré : relu")
	assert.Equal(t, 2, geo.reads)

	// SetRegionProbe invalide le cache.
	_, err := r.SetRegionProbe(ctx, "eu-west-gra", "https://probe.gra.example.com/healthz", "user:staff")
	require.NoError(t, err)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.gra.example.com", "https://probe2.lbv.example.com"}, o)
	assert.Equal(t, 3, geo.reads)

	// Cluster fermé (Update) : sa région n'est plus proposée.
	lbv := r.Store.(*memRegistry).cs["lbv-1"]
	lbv.Provider, lbv.Distribution, lbv.Name = domain.ProviderProxmox, domain.DistributionK8s, "lbv-1"
	closed := domain.StateClosed
	_, err = r.Update(ctx, "lbv-1", UpdateInput{State: &closed})
	require.NoError(t, err)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.gra.example.com"}, o)

	// Nouveau cluster (Declare) : sa région est proposée.
	mem.probes["af-west-2"] = "https://probe.abj.example.com/healthz"
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "abj-1", Provider: domain.ProviderProxmox, Distribution: domain.DistributionK8s, Region: "af-west-2"}, "user:staff")
	require.NoError(t, err)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.abj.example.com", "https://probe.gra.example.com"}, o)
}

// Appels simultanés sur cache vide : un seul calcul.
func TestProbeOrigins_Singleflight(t *testing.T) {
	ctx := context.Background()
	r, mem := geoRegistry(t)
	release := make(chan struct{})
	geo := &blockingGeo{memGeo: mem, release: release}
	r.Geo = geo
	mem.probes["af-central"] = "https://probe.lbv.example.com/healthz"

	var wg sync.WaitGroup
	results := make([][]string, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = r.ProbeOrigins(ctx)
		}()
	}
	// Laisse les appels rejoindre le calcul en cours avant de le débloquer.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, o := range results {
		assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)
	}
	assert.Equal(t, int32(1), geo.reads.Load())
}

// blockingGeo : RegionProbes attend release.
type blockingGeo struct {
	*memGeo
	release chan struct{}
	reads   atomic.Int32
}

func (b *blockingGeo) RegionProbes(ctx context.Context) (map[string]string, error) {
	b.reads.Add(1)
	<-b.release
	return b.memGeo.RegionProbes(ctx)
}
