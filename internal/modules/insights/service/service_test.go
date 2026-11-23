package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/insights/domain"
)

var (
	now     = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	project = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	all     = Access{Workloads: true, Builds: true, Budget: true, WorkloadDomains: true, SiteDomains: true, Databases: true, Addons: true}
)

type fakeWorkloads struct {
	list     []WorkloadState
	err      error
	restarts int
}

func (f *fakeWorkloads) List(context.Context, uuid.UUID) ([]WorkloadState, error) {
	return f.list, f.err
}
func (f *fakeWorkloads) Restarts(context.Context, uuid.UUID) (int, error) { return f.restarts, nil }

type fakeBuilds struct{ list []BuildState }

func (f fakeBuilds) List(context.Context, uuid.UUID) ([]BuildState, error) { return f.list, nil }

type fakeBudgets struct {
	b     *BudgetState
	calls int
}

func (f *fakeBudgets) Budget(context.Context, uuid.UUID) (*BudgetState, error) {
	f.calls++
	return f.b, nil
}

type fakeDisks struct {
	list []DiskState
	err  error
}

func (f fakeDisks) List(context.Context, uuid.UUID) ([]DiskState, error) { return f.list, f.err }

type fakeDomains map[uuid.UUID][]DomainState

func (f fakeDomains) List(_ context.Context, w uuid.UUID) ([]DomainState, error) { return f[w], nil }

type fakeSites struct {
	sites   []SiteState
	domains map[uuid.UUID][]DomainState
}

func (f fakeSites) List(context.Context, uuid.UUID) ([]SiteState, error) { return f.sites, nil }
func (f fakeSites) Domains(_ context.Context, s uuid.UUID) ([]DomainState, error) {
	return f.domains[s], nil
}

type fakeDatabases []DatabaseState

func (f fakeDatabases) List(context.Context, uuid.UUID) ([]DatabaseState, error) { return f, nil }

type fakeAddons struct {
	list []AddonState
	err  error
}

func (f fakeAddons) List(context.Context, uuid.UUID) ([]AddonState, error) { return f.list, f.err }

type fakeProjects struct{}

func (fakeProjects) Name(context.Context, uuid.UUID) (string, error) { return "boutique", nil }

type memCache struct {
	data map[string][]byte
	ttl  time.Duration
}

func (m *memCache) Get(_ context.Context, k string) ([]byte, bool, error) {
	v, ok := m.data[k]
	return v, ok, nil
}

func (m *memCache) Set(_ context.Context, k string, v []byte, ttl time.Duration) error {
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[k], m.ttl = v, ttl
	return nil
}

type logs struct{ n int }

func (l *logs) Warnw(string, ...any) { l.n++ }

func at(d time.Duration) time.Time { return now.Add(-d) }

func id(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }

func fixture() (*Service, *fakeBudgets) {
	web, api := id(1), id(2)
	budgets := &fakeBudgets{b: &BudgetState{Limit: 10000, Spent: 8500}}
	return &Service{
		Now: func() time.Time { return now },
		Workloads: &fakeWorkloads{restarts: 9, list: []WorkloadState{
			{ID: web, Name: "web", Status: "CRASHLOOP", UpdatedAt: at(5 * time.Minute)},
			{ID: api, Name: "api", Status: "RUNNING", UpdatedAt: at(time.Hour)},
			{ID: id(3), Name: "oom", Status: "OOM_KILLED", UpdatedAt: at(10 * time.Minute)},
			{ID: id(4), Name: "pull", Status: "IMAGE_PULL_FAILED", UpdatedAt: at(2 * time.Minute)},
			{ID: id(5), Name: "cron", Status: "RUN_FAILED", UpdatedAt: at(3 * time.Minute)},
			{ID: id(6), Name: "cache", Status: "CRASHLOOP", ManagedBy: "addon/redis/x"}, // porteur : vu comme service
		}},
		Builds: fakeBuilds{list: []BuildState{
			{ID: id(10), Name: "web", Status: "FAILED", Failure: "BUILD_FAILED", WorkloadID: &web, CreatedAt: at(20 * time.Minute)},
			{ID: id(11), Name: "web", Status: "SUCCEEDED", WorkloadID: &web, CreatedAt: at(2 * time.Hour)},
			{ID: id(12), Name: "api", Status: "FAILED", WorkloadID: &api, CreatedAt: at(3 * time.Hour)},
			{ID: id(13), Name: "api", Status: "SUCCEEDED", WorkloadID: &api, CreatedAt: at(time.Hour)}, // succès plus récent
		}},
		Budgets: budgets,
		Disks: fakeDisks{list: []DiskState{
			{Kind: "database", TargetID: id(20), TargetName: "main", Used: 90, Capacity: 100, MeasuredAt: at(30 * time.Minute)},
			{Kind: "workload", TargetID: id(1), TargetName: "web", Used: 50, Capacity: 100},
		}},
		WorkloadDomains: fakeDomains{
			web: {{ID: id(30), Hostname: "shop.example.com", CreatedAt: at(2 * time.Hour)}, {ID: id(31), Hostname: "new.example.com", CreatedAt: at(10 * time.Minute)}},
			api: {{ID: id(32), Hostname: "api.example.com", Verified: true, CreatedAt: at(48 * time.Hour)}},
		},
		Sites: fakeSites{sites: []SiteState{{ID: id(40), Name: "docs"}},
			domains: map[uuid.UUID][]DomainState{id(40): {{ID: id(41), Hostname: "docs.example.com", CreatedAt: at(3 * time.Hour)}}}},
		Databases: fakeDatabases{{ID: id(20), Name: "main", Status: "STOPPED", Attached: 1}, {ID: id(21), Name: "ok", Status: "READY"}},
		Addons:    fakeAddons{list: []AddonState{{ID: "a1", Name: "cache", Status: "CRASHLOOP"}, {ID: "a2", Name: "search", Status: "RUNNING"}}},
		Projects:  fakeProjects{},
	}, budgets
}

func byKind(l *domain.InsightList) map[string][]domain.Insight {
	out := map[string][]domain.Insight{}
	for _, i := range l.Insights {
		out[i.Kind] = append(out[i.Kind], i)
	}
	return out
}

func TestInsights_Rules(t *testing.T) {
	s, _ := fixture()
	s.Workloads.(*fakeWorkloads).list = s.Workloads.(*fakeWorkloads).list[:2] // web + api : sous le plafond
	l, err := s.List(context.Background(), project, all)
	if err != nil {
		t.Fatal(err)
	}
	k := byKind(l)
	if c := k[domain.KindWorkloadCrashloop]; len(c) != 1 || c[0].Params["restarts"] != "9" || c[0].Key != "workload_crashloop:"+id(1).String() || c[0].Resource.Type != "workload" {
		t.Fatalf("boucle : %+v", c)
	}
	if b := k[domain.KindBuildFailed]; len(b) != 1 || b[0].Resource.ID != id(10).String() || b[0].Params["workload_id"] != id(1).String() || b[0].Resource.Type != "build" {
		t.Fatalf("build : %+v", b)
	}
	if b := k[domain.KindBudgetHigh]; len(b) != 1 || b[0].Params["percent"] != "85" || b[0].Resource.Name != "boutique" || b[0].Severity != domain.SeverityWarn {
		t.Fatalf("budget : %+v", b)
	}
	if d := k[domain.KindDiskHigh]; len(d) != 1 || d[0].Params["percent"] != "90" || d[0].Resource.Type != "database" {
		t.Fatalf("disque : %+v", d)
	}
	d := k[domain.KindDomainUnverified]
	if len(d) != 2 {
		t.Fatalf("domaines (plus d'une heure, non vérifiés) : %+v", d)
	}
	for _, x := range d {
		switch x.Params["hostname"] {
		case "shop.example.com":
			if x.Params["workload_id"] != id(1).String() {
				t.Fatalf("domaine de conteneur : %+v", x)
			}
		case "docs.example.com":
			if x.Params["site_id"] != id(40).String() {
				t.Fatalf("domaine de site : %+v", x)
			}
		default:
			t.Fatalf("domaine inattendu : %+v", x)
		}
	}
	if db := k[domain.KindDatabaseStopped]; len(db) != 1 || db[0].Severity != domain.SeverityDanger {
		t.Fatalf("base : %+v", db)
	}
	if a := k[domain.KindAddonUnhealthy]; len(a) != 1 || a[0].Resource.ID != "a1" || a[0].Params["status"] != "CRASHLOOP" {
		t.Fatalf("service : %+v", a)
	}
	// Tri : danger, puis warn, puis info ; à gravité égale le plus récent.
	for i := 1; i < len(l.Insights); i++ {
		a, b := l.Insights[i-1], l.Insights[i]
		if severityRank(a.Severity) > severityRank(b.Severity) {
			t.Fatalf("ordre de gravité : %+v", l.Insights)
		}
		if a.Severity == b.Severity && a.Since != nil && b.Since != nil && a.Since.Before(*b.Since) {
			t.Fatalf("plus récent d'abord : %+v puis %+v", a, b)
		}
	}
	if !l.GeneratedAt.Equal(now) {
		t.Fatalf("generated_at = %s", l.GeneratedAt)
	}
	raw, _ := json.Marshal(l.Insights[0])
	for _, field := range []string{`"key"`, `"kind"`, `"severity"`, `"resource"`, `"params"`} {
		if !json.Valid(raw) || !strings.Contains(string(raw), field) {
			t.Fatalf("contrat : %s sans %s", raw, field)
		}
	}
}

func severityRank(s string) int {
	return map[string]int{domain.SeverityDanger: 0, domain.SeverityWarn: 1, domain.SeverityInfo: 2}[s]
}

func TestInsights_Cap(t *testing.T) {
	s, _ := fixture()
	l, err := s.List(context.Background(), project, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Insights) != domain.MaxInsights {
		t.Fatalf("plafond : %d", len(l.Insights))
	}
	// Les « info » et les « warn » les plus anciens tombent en premier.
	for _, i := range l.Insights {
		if i.Severity == domain.SeverityInfo {
			t.Fatalf("info gardé avant des warn : %+v", l.Insights)
		}
	}
}

func TestInsights_BudgetWithoutPermission(t *testing.T) {
	s, budgets := fixture()
	a := all
	a.Budget = false
	l, _ := s.List(context.Background(), project, a)
	if k := byKind(l); len(k[domain.KindBudgetHigh])+len(k[domain.KindBudgetStopped]) != 0 || budgets.calls != 0 {
		t.Fatalf("budget sans la permission : lu %d fois, %+v", budgets.calls, l.Insights)
	}
	// Arrêté par le budget : budget_stopped seul.
	s, budgets = fixture()
	budgets.b = &BudgetState{Limit: 100, Spent: 120, Stopped: true}
	l, _ = s.List(context.Background(), project, all)
	k := byKind(l)
	if len(k[domain.KindBudgetStopped]) != 1 || len(k[domain.KindBudgetHigh]) != 0 {
		t.Fatalf("budget arrêté : %+v", l.Insights)
	}
	// Sous 80 % ou sans budget : rien.
	for _, b := range []*BudgetState{{Limit: 100, Spent: 79}, {}, nil} {
		s, budgets = fixture()
		budgets.b = b
		l, _ = s.List(context.Background(), project, all)
		if k := byKind(l); len(k[domain.KindBudgetHigh]) != 0 {
			t.Fatalf("budget %+v : %+v", b, k[domain.KindBudgetHigh])
		}
	}
}

func TestInsights_SourceErrorOmitted(t *testing.T) {
	s, _ := fixture()
	lg := &logs{}
	s.Logger = lg
	s.Workloads.(*fakeWorkloads).err = errors.New("base injoignable")
	s.Addons = fakeAddons{err: errors.New("boom")}
	s.Disks = fakeDisks{err: errors.New("boom")}
	l, err := s.List(context.Background(), project, all)
	if err != nil {
		t.Fatal(err)
	}
	k := byKind(l)
	if len(k[domain.KindWorkloadCrashloop])+len(k[domain.KindAddonUnhealthy])+len(k[domain.KindDiskHigh]) != 0 {
		t.Fatalf("source en erreur inventée : %+v", l.Insights)
	}
	if len(k[domain.KindBuildFailed]) != 1 || len(k[domain.KindDatabaseStopped]) != 1 {
		t.Fatalf("les autres sources restent : %+v", l.Insights)
	}
	if lg.n < 3 {
		t.Fatalf("sources omises non journalisées : %d", lg.n)
	}
	// Aucune source : liste vide (jamais null).
	empty := &Service{Now: func() time.Time { return now }}
	l, _ = empty.List(context.Background(), project, all)
	if raw, _ := json.Marshal(l); !strings.Contains(string(raw), `"insights":[]`) {
		t.Fatalf("liste vide : %s", raw)
	}
}

func TestInsights_CacheByProjectAndRights(t *testing.T) {
	s, budgets := fixture()
	c := &memCache{}
	s.Cache = c
	ctx := context.Background()
	first, _ := s.List(ctx, project, all)
	if c.ttl != CacheTTL || len(c.data) != 1 {
		t.Fatalf("mise en cache : %v %d", c.ttl, len(c.data))
	}
	// Relu depuis le cache : la source n'est pas rappelée.
	budgets.b = &BudgetState{Limit: 100, Spent: 100, Stopped: true}
	again, _ := s.List(ctx, project, all)
	if budgets.calls != 1 || len(again.Insights) != len(first.Insights) {
		t.Fatalf("cache ignoré : %d appels", budgets.calls)
	}
	// Droits différents : autre entrée, jamais le résultat d'autrui.
	noBudget := all
	noBudget.Budget = false
	other, _ := s.List(ctx, project, noBudget)
	if len(c.data) != 2 {
		t.Fatalf("une entrée par ensemble de droits : %d", len(c.data))
	}
	if k := byKind(other); len(k[domain.KindBudgetHigh])+len(k[domain.KindBudgetStopped]) != 0 {
		t.Fatalf("fuite du budget par le cache : %+v", other.Insights)
	}
	// Autre projet : autre entrée.
	if _, err := s.List(ctx, id(99), all); err != nil || len(c.data) != 3 {
		t.Fatalf("clé par projet : %d", len(c.data))
	}
	if all.Key() == noBudget.Key() {
		t.Fatal("empreintes de droits identiques")
	}
}
