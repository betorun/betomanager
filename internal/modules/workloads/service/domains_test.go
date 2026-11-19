package service

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/activities"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

type fakeDNS struct {
	txt   map[string][]string
	hosts map[string][]string
	ns    map[string][]string
}

func (f fakeDNS) LookupNS(_ context.Context, name string) ([]*net.NS, error) {
	out := []*net.NS{}
	for _, h := range f.ns[name] {
		out = append(out, &net.NS{Host: h})
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func (f fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}
func (f fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	if v, ok := f.hosts[host]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

type fakeDomains struct {
	repository.DomainStore
	rows     map[uuid.UUID]*domain.WorkloadDomain
	siteHost string
	// projects : projet de chaque conteneur (absent : un autre projet).
	projects map[uuid.UUID]uuid.UUID
}

func (f *fakeDomains) Create(_ context.Context, d *domain.WorkloadDomain) error {
	f.rows[d.ID] = d
	return nil
}
func (f *fakeDomains) List(_ context.Context, w uuid.UUID) ([]domain.WorkloadDomain, error) {
	out := []domain.WorkloadDomain{}
	for _, d := range f.rows {
		if d.WorkloadID == w {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (f *fakeDomains) Get(_ context.Context, w, id uuid.UUID) (*domain.WorkloadDomain, error) {
	if d, ok := f.rows[id]; ok && d.WorkloadID == w {
		c := *d
		return &c, nil
	}
	return nil, domain.ErrDomainNotFound
}
func (f *fakeDomains) Delete(_ context.Context, _, id uuid.UUID) error {
	delete(f.rows, id)
	return nil
}
func (f *fakeDomains) VerifiedRoutes(_ context.Context, w uuid.UUID) ([]domain.Route, error) {
	out := []domain.Route{}
	for _, d := range f.rows {
		if d.WorkloadID == w && d.Verified {
			out = append(out, domain.Route{Host: d.Hostname, Path: pathOr(d.Path)})
		}
	}
	return out, nil
}
func (f *fakeDomains) HostClaims(_ context.Context, host string, except uuid.UUID) ([]domain.HostClaim, error) {
	out := []domain.HostClaim{}
	for _, d := range f.rows {
		if d.Hostname == host && d.Verified && d.ID != except {
			c := *d
			c.Path = pathOr(c.Path)
			out = append(out, domain.HostClaim{WorkloadDomain: c, ProjectID: f.projects[d.WorkloadID]})
		}
	}
	return out, nil
}
func (f *fakeDomains) ProjectHostToken(_ context.Context, project uuid.UUID, host string) (string, bool, error) {
	pending := ""
	for _, d := range f.rows {
		if d.Hostname != host || f.projects[d.WorkloadID] != project {
			continue
		}
		if d.Verified {
			return d.Token, true, nil
		}
		pending = d.Token
	}
	return pending, false, nil
}

func pathOr(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func (f *fakeDomains) SiteHostnameTaken(_ context.Context, host string) (bool, error) {
	return host == f.siteHost, nil
}
func (f *fakeDomains) SetVerified(_ context.Context, id uuid.UUID, v bool) error {
	f.rows[id].Verified = v
	return nil
}
func (f *fakeDomains) Touch(context.Context, uuid.UUID) error { return nil }

const domainTarget = "203.0.113.10"

func domainService(t *testing.T) (*WorkloadService, *fakeDomains, *fakeDNS, *fakeDistributor, uuid.UUID) {
	t.Helper()
	saved := configs.AppConfig
	t.Cleanup(func() { configs.AppConfig = saved })
	configs.AppConfig.Vps.AdressIp = domainTarget
	id := uuid.New()
	repo := &fakeRepo{w: domain.Workload{ID: id, ProjectID: uuid.New(), Name: "web", Namespace: "km-x", Image: "nginx:1", Status: "RUNNING", TargetPort: 8080}}
	dns := &fakeDNS{txt: map[string][]string{}, hosts: map[string][]string{}, ns: map[string][]string{}}
	store := &fakeDomains{rows: map[uuid.UUID]*domain.WorkloadDomain{}, projects: map[uuid.UUID]uuid.UUID{id: repo.w.ProjectID}}
	dist := &fakeDistributor{}
	return &WorkloadService{Repo: repo, Distributor: dist, Variables: &fakeVariables{}, Domains: store,
		DomainCfg: &DomainSettings{Resolver: dns, Target: domainTarget, TLS: true}}, store, dns, dist, id
}

// Sans la preuve TXT, pointer vers la plateforme ne donne rien : sinon un
// client captait n'importe quel domaine qui nous vise déjà.
func TestVerifyDomain_PointingIsNotProof(t *testing.T) {
	s, store, dns, dist, id := domainService(t)
	d, err := s.AddDomain(context.Background(), id, "WWW.Exemple.SN.", "")
	require.NoError(t, err)
	assert.Equal(t, "www.exemple.sn", d.Hostname)
	assert.Empty(t, dist.started, "ajouter ne route rien")
	require.Len(t, d.Records, 2)
	assert.Equal(t, domain.DNSRecord{Type: "TXT", Name: "_betorun-challenge.www.exemple.sn", Host: "_betorun-challenge.www", Value: store.rows[d.ID].Token}, d.Records[0])
	assert.Equal(t, domain.DNSRecord{Type: "A", Name: "www.exemple.sn", Host: "www", Value: domainTarget}, d.Records[1])
	assert.Equal(t, "exemple.sn", d.Zone)
	assert.False(t, d.Apex)
	assert.Equal(t, "/", d.Path)

	dns.hosts["www.exemple.sn"] = []string{domainTarget}
	v, err := s.VerifyDomain(context.Background(), id, d.ID)
	require.NoError(t, err)
	assert.False(t, v.Verified)
	assert.True(t, v.Pointing)
	assert.Empty(t, dist.started)

	// Le jeton d'une autre demande ne compte pas.
	dns.txt["_betorun-challenge.www.exemple.sn"] = []string{"betorun-verify=autre"}
	v, err = s.VerifyDomain(context.Background(), id, d.ID)
	require.NoError(t, err)
	assert.False(t, v.Verified)
	require.NotNil(t, v.Check, "diagnostic après vérification")
	assert.Equal(t, []string{"betorun-verify=autre"}, v.Check.TXTFound)
	assert.False(t, v.Check.ProofFound)
	assert.Equal(t, "_betorun-challenge.www.exemple.sn", v.Check.Name)
	assert.Equal(t, DefaultDomainDNSServers, v.Check.Resolver)
	assert.True(t, v.HTTPS)

	dns.txt["_betorun-challenge.www.exemple.sn"] = []string{store.rows[d.ID].Token}
	v, err = s.VerifyDomain(context.Background(), id, d.ID)
	require.NoError(t, err)
	assert.True(t, v.Verified)
	require.Len(t, dist.started, 1, "preuve faite : nouveau déploiement")
	in := dist.args[0].(workflows.DeployWorkloadInput)
	assert.Equal(t, []activities.DomainRoute{{Host: "www.exemple.sn", Path: "/"}}, in.CustomRoutes)
	assert.True(t, in.CustomDomainTLS)
}

func TestAddDomain_Invalid(t *testing.T) {
	s, _, _, _, id := domainService(t)
	configs.AppConfig.Sites.BaseDomain = "betorun.app"
	for _, bad := range []string{"", "localhost", "10.0.0.1", "*.exemple.sn", "exemple.sn/x", "a b.sn", "x.betorun.app", "-a.sn"} {
		_, err := s.AddDomain(context.Background(), id, bad, "")
		assert.Error(t, err, bad)
	}
}

func TestAddDomain_Limit(t *testing.T) {
	s, store, _, _, id := domainService(t)
	for i := 0; i < domain.MaxDomainsPerWorkload; i++ {
		u := uuid.New()
		store.rows[u] = &domain.WorkloadDomain{ID: u, WorkloadID: id, Hostname: u.String() + ".sn"}
	}
	_, err := s.AddDomain(context.Background(), id, "www.exemple.sn", "")
	assert.ErrorIs(t, err, domain.ErrDomainLimit)
}

func TestVerifyDomain_Conflicts(t *testing.T) {
	const host, challenge = "www.exemple.sn", "_betorun-challenge.www.exemple.sn"
	ctx := context.Background()

	// Preuve de l'ancien détenteur toujours présente : refus.
	s, store, dns, dist, id := domainService(t)
	d, err := s.AddDomain(ctx, id, host, "")
	require.NoError(t, err)
	other := &domain.WorkloadDomain{ID: uuid.New(), WorkloadID: uuid.New(), Hostname: host, Token: "betorun-verify=ancien", Verified: true}
	store.rows[other.ID] = other
	dns.txt[challenge] = []string{store.rows[d.ID].Token, other.Token}
	_, err = s.VerifyDomain(ctx, id, d.ID)
	assert.ErrorIs(t, err, domain.ErrDomainClaimed)
	assert.True(t, other.Verified)
	assert.Empty(t, dist.started)

	// Preuve de l'ancien détenteur disparue : le domaine a changé de mains.
	dns.txt[challenge] = []string{store.rows[d.ID].Token}
	v, err := s.VerifyDomain(ctx, id, d.ID)
	require.NoError(t, err)
	assert.True(t, v.Verified)
	assert.False(t, other.Verified, "route retirée à l'ancien détenteur")

	// Nom pris par un site statique.
	s, store, dns, _, id = domainService(t)
	store.siteHost = host
	d, err = s.AddDomain(ctx, id, host, "")
	require.NoError(t, err)
	dns.txt[challenge] = []string{store.rows[d.ID].Token}
	_, err = s.VerifyDomain(ctx, id, d.ID)
	assert.ErrorIs(t, err, domain.ErrDomainClaimed)

	// Un domaine d'un autre conteneur : introuvable.
	_, err = s.VerifyDomain(ctx, uuid.New(), d.ID)
	assert.ErrorIs(t, err, domain.ErrDomainNotFound)
}

// Preuve retirée après coup : le domaine reste routé (une erreur DNS passagère
// ne doit pas couper un service), pas de nouveau déploiement.
func TestVerifyDomain_KeepsRouteWhenProofDisappears(t *testing.T) {
	s, store, _, dist, id := domainService(t)
	u := uuid.New()
	store.rows[u] = &domain.WorkloadDomain{ID: u, WorkloadID: id, Hostname: "www.exemple.sn", Token: "t", Verified: true}
	v, err := s.VerifyDomain(context.Background(), id, u)
	require.NoError(t, err)
	assert.True(t, v.Verified)
	assert.Empty(t, dist.started)
}

func TestRemoveDomain(t *testing.T) {
	s, store, _, dist, id := domainService(t)
	u := uuid.New()
	store.rows[u] = &domain.WorkloadDomain{ID: u, WorkloadID: id, Hostname: "a.sn"}
	require.NoError(t, s.RemoveDomain(context.Background(), id, u))
	assert.Empty(t, dist.started, "jamais routé : rien à redéployer")

	u = uuid.New()
	store.rows[u] = &domain.WorkloadDomain{ID: u, WorkloadID: id, Hostname: "b.sn", Verified: true}
	require.NoError(t, s.RemoveDomain(context.Background(), id, u))
	assert.Len(t, dist.started, 1, "route à retirer")
	assert.Empty(t, store.rows)
}

func TestDomainRecords_ApexAndTargets(t *testing.T) {
	ctx := context.Background()

	// Cible IP : A, y compris au sommet (« @ »).
	s, _, _, _, id := domainService(t)
	d, err := s.AddDomain(ctx, id, "exemple.sn", "")
	require.NoError(t, err)
	assert.True(t, d.Apex)
	assert.Equal(t, domain.DNSRecord{Type: "A", Name: "exemple.sn", Host: "@", Value: domainTarget}, d.Records[1])

	// Cible nom : CNAME pour un sous-domaine, A (adresses de la cible) + ALIAS
	// en alternative au sommet.
	s, _, dns, _, id := domainService(t)
	s.DomainCfg.Target = "edge.betorun.app"
	dns.hosts["edge.betorun.app"] = []string{"203.0.113.10", "203.0.113.11", "2001:db8::1"}
	sub, err := s.AddDomain(ctx, id, "app.exemple.co.uk", "")
	require.NoError(t, err)
	assert.Equal(t, "exemple.co.uk", sub.Zone)
	assert.Equal(t, domain.DNSRecord{Type: "CNAME", Name: "app.exemple.co.uk", Host: "app", Value: "edge.betorun.app"}, sub.Records[1])
	apex, err := s.AddDomain(ctx, id, "exemple.sn", "")
	require.NoError(t, err)
	require.Len(t, apex.Records, 4, "TXT, deux A (IPv4 seulement), ALIAS")
	assert.Equal(t, "A", apex.Records[1].Type)
	assert.Equal(t, "203.0.113.11", apex.Records[2].Value)
	assert.Equal(t, domain.DNSRecord{Type: "ALIAS", Name: "exemple.sn", Host: "@", Value: "edge.betorun.app", Alternative: true}, apex.Records[3])
}

func TestDomain_DNSProviderDetected(t *testing.T) {
	s, _, dns, _, id := domainService(t)
	dns.ns["exemple.sn"] = []string{"ada.ns.cloudflare.com.", "bob.ns.cloudflare.com."}
	d, err := s.AddDomain(context.Background(), id, "www.exemple.sn", "")
	require.NoError(t, err)
	assert.Equal(t, "cloudflare", d.DNSProvider)
	list, err := s.ListDomains(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "cloudflare", list[0].DNSProvider)
}

// Keycloak sur /auth, le site sur / : même nom, deux conteneurs du projet. La
// preuve du nom est reprise, la route « / » de l'autre conteneur reste.
func TestDomain_PathSharesProofInProject(t *testing.T) {
	ctx := context.Background()
	const host, challenge = "backoffice.exemple.sn", "_betorun-challenge.backoffice.exemple.sn"
	s, store, dns, dist, id := domainService(t)
	project := store.projects[id]
	site := uuid.New()
	store.projects[site] = project
	root := &domain.WorkloadDomain{ID: uuid.New(), WorkloadID: site, Hostname: host, Path: "/", Token: "betorun-verify=site", Verified: true}
	store.rows[root.ID] = root
	dns.txt[challenge] = []string{root.Token}

	d, err := s.AddDomain(ctx, id, host, "/auth/")
	require.NoError(t, err)
	assert.Equal(t, "/auth", d.Path)
	assert.True(t, d.SharedProof)
	assert.Equal(t, root.Token, store.rows[d.ID].Token, "même preuve : rien à créer chez le registraire")

	v, err := s.VerifyDomain(ctx, id, d.ID)
	require.NoError(t, err)
	assert.True(t, v.Verified)
	assert.True(t, root.Verified, "le site garde « / »")
	in := dist.args[0].(workflows.DeployWorkloadInput)
	assert.Equal(t, []activities.DomainRoute{{Host: host, Path: "/auth"}}, in.CustomRoutes)

	// Même chemin : la route passe d'un conteneur du projet à l'autre.
	again, err := s.AddDomain(ctx, id, host, "/")
	require.NoError(t, err)
	_, err = s.VerifyDomain(ctx, id, again.ID)
	require.NoError(t, err)
	assert.False(t, root.Verified, "« / » déplacé vers ce conteneur")
}

func TestAddDomain_InvalidPath(t *testing.T) {
	s, _, _, _, id := domainService(t)
	for _, bad := range []string{"auth", "/a/../b", "/a b", "/a?x=1", "/" + string(make([]byte, 120))} {
		_, err := s.AddDomain(context.Background(), id, "www.exemple.sn", bad)
		assert.ErrorIs(t, err, domain.ErrInvalidPath, bad)
	}
}

// Deux chemins du même nom ajoutés avant toute vérification : un seul TXT.
func TestDomain_PendingProofShared(t *testing.T) {
	ctx := context.Background()
	const host = "backoffice.exemple.sn"
	s, store, _, _, id := domainService(t)
	site := uuid.New()
	store.projects[site] = store.projects[id]
	first := &domain.WorkloadDomain{ID: uuid.New(), WorkloadID: site, Hostname: host, Path: "/", Token: "betorun-verify=premier"}
	store.rows[first.ID] = first

	d, err := s.AddDomain(ctx, id, host, "/auth")
	require.NoError(t, err)
	assert.Equal(t, first.Token, store.rows[d.ID].Token, "même jeton que la demande en attente")
	assert.False(t, d.SharedProof, "preuve pas encore en place : les enregistrements restent affichés")
}
