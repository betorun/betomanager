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

// memGeo : catalogue des régions et table pays → région en mémoire.
type memGeo struct {
	countries map[string]domain.CountryRegion
	regions   map[string]domain.RegionSetting
}

func newMemGeo() *memGeo {
	return &memGeo{countries: map[string]domain.CountryRegion{}, regions: map[string]domain.RegionSetting{}}
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
	for k, v := range m.regions {
		if v.ProbeURL != "" {
			out[k] = v.ProbeURL
		}
	}
	return out, nil
}
func (m *memGeo) RegionCatalog(context.Context) ([]domain.RegionSetting, error) {
	out := []domain.RegionSetting{}
	for _, e := range m.regions {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}
func (m *memGeo) CreateRegion(_ context.Context, e *domain.RegionSetting) error {
	if _, ok := m.regions[e.Code]; ok {
		return domain.ErrRegionExists
	}
	m.regions[e.Code] = *e
	return nil
}
func (m *memGeo) SaveRegion(_ context.Context, e *domain.RegionSetting) error {
	if _, ok := m.regions[e.Code]; !ok {
		return domain.ErrUnknownRegion
	}
	m.regions[e.Code] = *e
	return nil
}
func (m *memGeo) DeleteRegion(_ context.Context, code string) error {
	delete(m.regions, code)
	return nil
}

// setProbe : adresse de mesure écrite telle quelle (sans contrôle, comme
// une ligne en base), région créée au besoin.
func (m *memGeo) setProbe(code, url string) {
	e := m.regions[code]
	e.Code, e.ProbeURL = code, url
	m.regions[code] = e
}

// probe : adresse de mesure posée par le service (comme le back-office).
func probe(t *testing.T, r *Registry, code, url string) {
	t.Helper()
	_, err := r.UpdateRegion(context.Background(), code, RegionPatch{ProbeURL: &url}, "user:staff")
	require.NoError(t, err)
}

// geoRegistry : défaut en eu-west-gra, un cluster ouvert en af-central, un
// fermé en af-west (région connue, pas proposée) ; les trois au catalogue,
// plus af-east sans cluster.
func geoRegistry(t *testing.T) (*Registry, *memGeo) {
	t.Helper()
	ctx := context.Background()
	store := newMemRegistry()
	r := newRegistry(store, nil)
	geo := newMemGeo()
	geo.regions["eu-west-gra"] = domain.RegionSetting{Code: "eu-west-gra", Zone: "eu-west", Country: "FR", City: "Gravelines"}
	geo.regions["af-central"] = domain.RegionSetting{Code: "af-central", Zone: "af-central", Country: "CG", City: "Brazzaville"}
	geo.regions["af-west"] = domain.RegionSetting{Code: "af-west"}
	geo.regions["af-east"] = domain.RegionSetting{Code: "af-east", Zone: "af-east", Country: "KE", City: "Nairobi"}
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
	probe(t, r, "af-central", "https://lbv.example.test/healthz")

	// Pays associé : sa région est suggérée et passe en tête, avec zone,
	// pays et ville du catalogue.
	rs, err := r.Regions(ctx, "GA")
	require.NoError(t, err)
	assert.Equal(t, []string{"af-central", "eu-west-gra"}, codes(rs))
	assert.True(t, rs[0].Suggested)
	assert.False(t, rs[1].Suggested)
	assert.True(t, rs[1].Default, "le défaut ne change pas")
	assert.Equal(t, "https://lbv.example.test/healthz", rs[0].ProbeURL)
	assert.Equal(t, [3]string{"af-central", "CG", "Brazzaville"}, [3]string{rs[0].Zone, rs[0].Country, rs[0].City})
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

	// Région associée mais pas proposée (cluster fermé, ou aucun) : rien de suggéré.
	for _, region := range []string{"af-west", "af-east"} {
		_, err = r.SetCountryRegion(ctx, "SN", region, "")
		require.NoError(t, err)
		rs, err = r.Regions(ctx, "SN")
		require.NoError(t, err)
		assert.Equal(t, []string{"eu-west-gra", "af-central"}, codes(rs), region)
	}
}

func TestCountryRegions_Admin(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)

	e, err := r.SetCountryRegion(ctx, " cg ", "af-central", "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.CountryRegion{Country: "CG", Region: "af-central", UpdatedBy: "user:staff"}, *e)
	_, err = r.SetCountryRegion(ctx, "GA", "af-west", "")
	require.NoError(t, err, "région d'un cluster fermé : au catalogue, acceptée")
	_, err = r.SetCountryRegion(ctx, "KE", "af-east", "")
	require.NoError(t, err, "région sans cluster : au catalogue, acceptée")

	_, err = r.SetCountryRegion(ctx, "GA", "ap-south", "")
	assert.ErrorIs(t, err, domain.ErrUnknownRegion, "hors catalogue")
	_, err = r.SetCountryRegion(ctx, "GA", "AF CENTRAL", "")
	assert.ErrorIs(t, err, domain.ErrInvalidRegionCode)
	for _, bad := range []string{"XX", "", "GAB"} {
		_, err = r.SetCountryRegion(ctx, bad, "af-central", "")
		assert.ErrorIs(t, err, country.ErrInvalid, bad)
	}

	list, err := r.CountryRegions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "CG", list[0].Country)
	assert.Equal(t, "GA", list[1].Country)

	require.NoError(t, r.DeleteCountryRegion(ctx, "ga"))
	require.NoError(t, r.DeleteCountryRegion(ctx, "GA"), "absente : rien")
	assert.ErrorIs(t, r.DeleteCountryRegion(ctx, "XX"), country.ErrInvalid)
	list, err = r.CountryRegions(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestRegionCatalog_List(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)
	_, err := r.SetCountryRegion(ctx, "CG", "af-central", "")
	require.NoError(t, err)
	_, err = r.SetCountryRegion(ctx, "GA", "af-central", "")
	require.NoError(t, err)
	// Cluster dont la région manque au catalogue (d'avant 000082).
	r.Store.(*memRegistry).cs["dkr-1"].Region = "af-old"

	list, err := r.RegionSettings(ctx)
	require.NoError(t, err)
	got := map[string][2]int{}
	var order []string
	for _, e := range list {
		got[e.Code] = [2]int{e.Clusters, e.Countries}
		order = append(order, e.Code)
	}
	assert.Equal(t, []string{"af-central", "af-east", "af-old", "af-west", "eu-west-gra"}, order, "triées par code")
	assert.Equal(t, [2]int{1, 2}, got["af-central"])
	assert.Equal(t, [2]int{0, 0}, got["af-east"])
	assert.Equal(t, [2]int{1, 0}, got["af-old"], "région d'un cluster hors catalogue : listée, à compléter")
	assert.Equal(t, [2]int{0, 0}, got["af-west"])
}

func TestRegionCatalog_Create(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)

	e, err := r.CreateRegion(ctx, RegionInput{Code: " af-west-abj ", Zone: "af-west", Country: "ci", City: "  Abidjan  ",
		ProbeURL: "https://abj.example.test/healthz"}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.RegionSetting{Code: "af-west-abj", Zone: "af-west", Country: "CI", City: "Abidjan",
		ProbeURL: "https://abj.example.test/healthz", UpdatedBy: "user:staff"}, *e)

	_, err = r.CreateRegion(ctx, RegionInput{Code: "af-west-abj", Zone: "af-west", Country: "CI", City: "Abidjan"}, "")
	assert.ErrorIs(t, err, domain.ErrRegionExists)
	for name, in := range map[string]RegionInput{
		"code":    {Code: "AF West", Zone: "af-west", Country: "CI", City: "Abidjan"},
		"zone":    {Code: "x-1", Zone: "ap-south", Country: "CI", City: "Abidjan"},
		"pays":    {Code: "x-1", Zone: "af-west", Country: "XX", City: "Abidjan"},
		"ville":   {Code: "x-1", Zone: "af-west", Country: "CI", City: "Abidjan;drop"},
		"vide":    {Code: "x-1", Zone: "af-west", Country: "CI", City: "  "},
		"adresse": {Code: "x-1", Zone: "af-west", Country: "CI", City: "Abidjan", ProbeURL: "http://abj.example.test"},
	} {
		_, err = r.CreateRegion(ctx, in, "")
		assert.Error(t, err, name)
	}
	_, err = r.CreateRegion(ctx, RegionInput{Code: "af-central-lbv", Zone: "af-central", Country: "GA", City: "N’Djolé-Ville"}, "")
	assert.NoError(t, err, "accents, apostrophe typographique et tiret acceptés")
}

func TestRegionCatalog_Update(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)
	s := func(v string) *string { return &v }

	e, err := r.UpdateRegion(ctx, "af-central", RegionPatch{City: s("Pointe-Noire")}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, [3]string{"af-central", "CG", "Pointe-Noire"}, [3]string{e.Zone, e.Country, e.City}, "le reste inchangé")

	// Région reprise, à compléter : zone, pays et ville ensemble.
	_, err = r.UpdateRegion(ctx, "af-west", RegionPatch{City: s("Dakar")}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidRegionZone, "zone encore vide")
	e, err = r.UpdateRegion(ctx, "af-west", RegionPatch{Zone: s("af-west"), Country: s("sn"), City: s("Dakar")}, "")
	require.NoError(t, err)
	assert.Equal(t, "SN", e.Country)
	// Adresse seule : possible sans compléter la région.
	r2, _ := geoRegistry(t)
	e, err = r2.UpdateRegion(ctx, "af-west", RegionPatch{ProbeURL: s(" https://dkr.example.test/healthz ")}, "")
	require.NoError(t, err)
	assert.Equal(t, "https://dkr.example.test/healthz", e.ProbeURL)
	assert.Empty(t, e.Zone)

	_, err = r.UpdateRegion(ctx, "af-central", RegionPatch{ProbeURL: s("http://x.example.test")}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidProbeURL, "https obligatoire")
	_, err = r.UpdateRegion(ctx, "ap-south", RegionPatch{City: s("Mumbai")}, "")
	assert.ErrorIs(t, err, domain.ErrUnknownRegion)
	e, err = r.UpdateRegion(ctx, "af-central", RegionPatch{ProbeURL: s("")}, "")
	require.NoError(t, err, "vide : retirée")
	assert.Empty(t, e.ProbeURL)
}

func TestRegionCatalog_Delete(t *testing.T) {
	ctx := context.Background()
	r, geo := geoRegistry(t)

	assert.ErrorIs(t, r.DeleteRegion(ctx, "af-central"), domain.ErrRegionInUse, "portée par un cluster")
	_, err := r.SetCountryRegion(ctx, "KE", "af-east", "")
	require.NoError(t, err)
	assert.ErrorIs(t, r.DeleteRegion(ctx, "af-east"), domain.ErrRegionInUse, "suggérée pour un pays")
	require.NoError(t, r.DeleteCountryRegion(ctx, "KE"))
	require.NoError(t, r.DeleteRegion(ctx, "af-east"))
	assert.NotContains(t, geo.regions, "af-east")
	assert.ErrorIs(t, r.DeleteRegion(ctx, "af-east"), domain.ErrUnknownRegion)
}

// La région d'un cluster vient du catalogue.
func TestRegionCatalog_ClusterRegion(t *testing.T) {
	ctx := context.Background()
	r, _ := geoRegistry(t)
	s := func(v string) *string { return &v }

	_, err := r.Declare(ctx, DeclareInput{ClusterID: "nbo-1", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s, Region: "af-nairobi"}, "")
	assert.ErrorIs(t, err, domain.ErrUnknownRegion)
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "nbo-1", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s, Region: "af-east"}, "")
	require.NoError(t, err)
	_, err = r.Declare(ctx, DeclareInput{ClusterID: "nbo-2", Provider: domain.ProviderOVH, Distribution: domain.DistributionK3s}, "")
	require.NoError(t, err, "sans région : accepté")

	_, err = r.Update(ctx, "nbo-1", UpdateInput{Region: s("ap-south")})
	assert.ErrorIs(t, err, domain.ErrUnknownRegion)
	_, err = r.Update(ctx, "nbo-1", UpdateInput{Region: s("af-central")})
	require.NoError(t, err)

	// Cluster resté sur une région hors catalogue : ses autres champs se modifient.
	r.Store.(*memRegistry).cs["nbo-1"].Region = "af-old"
	_, err = r.Update(ctx, "nbo-1", UpdateInput{Name: s("Nairobi 1")})
	require.NoError(t, err)
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
	geo.setProbe("af-central", "https://Probe.LBV.example.com/healthz")
	geo.setProbe("eu-west-gra", "https://probe.gra.example.com:8443/ping?x=1")
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
	geo.setProbe("af-central", "https://probe.lbv.example.com/healthz")
	geo.setProbe("eu-west-gra", "https://probe.lbv.example.com/autre")
	geo.setProbe("af-west", "https://probe.dkr.example.com/healthz")
	geo.setProbe("ap-south", "https://probe.ap.example.com/healthz")
	o, err := r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)

	// Cluster supprimé : sa région disparaît au prochain calcul.
	require.NoError(t, r.Delete(ctx, "lbv-1"))
	o, err = r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o, "toujours celle d'eu-west-gra")
	geo.setProbe("eu-west-gra", "")
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
	geo.setProbe("af-central", "https://a.com;sandbox/x")
	geo.setProbe("eu-west-gra", "http://insecure.example.com/healthz")
	o, err := r.ProbeOrigins(ctx)
	require.NoError(t, err)
	assert.Empty(t, o)

	geo.setProbe("eu-west-gra", "https://*")
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
// UpdateRegion ou une écriture du registre.
func TestProbeOrigins_Cache(t *testing.T) {
	ctx := context.Background()
	r, mem := geoRegistry(t)
	geo := &countingGeo{memGeo: mem}
	r.Geo = geo
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	mem.setProbe("af-central", "https://probe.lbv.example.com/healthz")

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
	mem.setProbe("af-central", "https://probe2.lbv.example.com/healthz")
	now = now.Add(probeOriginsTTL - time.Second)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe.lbv.example.com"}, o)
	now = now.Add(time.Second)
	o, _ = r.ProbeOrigins(ctx)
	assert.Equal(t, []string{"https://probe2.lbv.example.com"}, o, "expiré : relu")
	assert.Equal(t, 2, geo.reads)

	// UpdateRegion invalide le cache.
	url := "https://probe.gra.example.com/healthz"
	_, err := r.UpdateRegion(ctx, "eu-west-gra", RegionPatch{ProbeURL: &url}, "user:staff")
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
	mem.setProbe("af-west-2", "https://probe.abj.example.com/healthz")
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
	mem.setProbe("af-central", "https://probe.lbv.example.com/healthz")

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
