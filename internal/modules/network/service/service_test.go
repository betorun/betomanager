package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/network/domain"
	wlDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

type fakeWorkloads []wlDomain.Workload

func (f fakeWorkloads) ListByProject(context.Context, uuid.UUID) ([]wlDomain.Workload, error) {
	return f, nil
}

type fakeDomains map[uuid.UUID][]wlDomain.WorkloadDomain

func (f fakeDomains) List(_ context.Context, w uuid.UUID) ([]wlDomain.WorkloadDomain, error) {
	return f[w], nil
}

type fakeDBs struct {
	dbs  []dbDomain.Database
	atts map[uuid.UUID][]dbDomain.Attachment
}

func (f fakeDBs) List(context.Context, uuid.UUID) ([]dbDomain.Database, error) { return f.dbs, nil }
func (f fakeDBs) ListAttachments(_ context.Context, id uuid.UUID) ([]dbDomain.Attachment, error) {
	return f.atts[id], nil
}

func TestGet_ProjectNetwork(t *testing.T) {
	api, cache, jobs, nightly, hidden := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	db := uuid.New()
	wls := fakeWorkloads{
		{ID: api, Name: "api", Kind: "", Status: "RUNNING", ExternalURL: "https://api-37f628.203.0.113.10.sslip.io",
			Spec: []byte(`{"wait_for":["db-main-rw:5432","cache:6379"]}`)},
		{ID: cache, Name: "cache", Kind: wlDomain.KindService, TargetPort: 6379, Status: "RUNNING", ExternalURL: "ignored"},
		{ID: jobs, Name: "jobs", Kind: wlDomain.KindWorker, Status: "RUNNING", Spec: []byte(`{"wait_for":["cache.km-x.svc:6379","nope:1"]}`)},
		{ID: nightly, Name: "nightly", Kind: wlDomain.KindCron, Status: "RUNNING"},
		{ID: hidden, Name: "admin", Kind: wlDomain.KindWeb, Status: "RUNNING"}, // sans adresse publique
	}
	s := &Service{Workloads: wls, DomainTLS: true,
		Domains: fakeDomains{api: {{Hostname: "api.client.sn", Path: "/", Verified: true}, {Hostname: "pending.client.sn", Verified: false}},
			hidden: {{Hostname: "client.sn", Path: "/admin", Verified: true}}},
		Databases: fakeDBs{dbs: []dbDomain.Database{{ID: db, Name: "main", Status: "READY"}},
			atts: map[uuid.UUID][]dbDomain.Attachment{db: {{DatabaseID: db, WorkloadID: api, Variable: "DATABASE_URL"}}}},
	}
	n, err := s.Get(context.Background(), uuid.New())
	require.NoError(t, err)

	names := []string{}
	for _, m := range n.Members {
		names = append(names, m.Kind+":"+m.Name)
	}
	assert.Equal(t, []string{"web:admin", "web:api", "service:cache", "worker:jobs", "cron:nightly", "database:main"}, names)

	by := map[string]domain.Member{}
	for _, m := range n.Members {
		by[m.Name] = m
	}
	assert.Equal(t, "http://api", by["api"].Internal.URL)
	assert.Equal(t, []domain.PublicEndpoint{{URL: "https://api-37f628.203.0.113.10.sslip.io", Source: domain.PublicAutomatic},
		{URL: "https://api.client.sn", Source: domain.PublicDomain}}, by["api"].Public, "domaine non vérifié : pas routé")
	assert.Equal(t, "cache:6379", by["cache"].Internal.URL)
	assert.Empty(t, by["cache"].Public, "un service n'est jamais public")
	assert.Nil(t, by["jobs"].Internal, "un worker n'écoute rien")
	assert.Equal(t, []domain.PublicEndpoint{{URL: "https://client.sn/admin", Source: domain.PublicDomain}}, by["admin"].Public)
	assert.Equal(t, "db-main-rw:5432", by["main"].Internal.URL)

	assert.ElementsMatch(t, []domain.Link{
		{From: api.String(), To: db.String(), Kind: domain.LinkDatabase, Variable: "DATABASE_URL"},
		{From: api.String(), To: cache.String(), Kind: domain.LinkWaitsFor},
		{From: jobs.String(), To: cache.String(), Kind: domain.LinkWaitsFor},
	}, n.Links, "base branchée : un seul lien ; adresse inconnue ignorée ; forme longue ramenée au nom")
	assert.Equal(t, domain.PolicyRules(), n.Rules)
}

func TestGet_EmptyProject(t *testing.T) {
	n, err := (&Service{Workloads: fakeWorkloads{}}).Get(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.NotNil(t, n.Members)
	assert.NotNil(t, n.Links)
	assert.Len(t, n.Rules, 7)
}

// Garde-fou : la politique décrite ne s'élargit pas sans revue (tout
// changement doit être appliqué par chaque implémentation).
func TestPolicyRules_Stable(t *testing.T) {
	allowed := map[string]bool{}
	for _, r := range domain.PolicyRules() {
		allowed[r.Code] = r.Allowed
	}
	for _, r := range domain.PolicyRules() {
		if r.Code == "egress_internet" {
			assert.Equal(t, []int{80, 443, 465, 587, 3306, 5432, 6379, 27017}, r.PortList, "jamais le 25")
		}
	}
	assert.Equal(t, map[string]bool{"project_internal": true, "project_to_databases": true, "internet_to_public_web": true,
		"internet_to_internal": false, "other_projects": false, "egress_internet": true, "egress_private": false}, allowed)
}
