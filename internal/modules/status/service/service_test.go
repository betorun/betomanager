package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
)

type memStore struct {
	incidents map[uuid.UUID]*domain.Incident
}

func (m *memStore) CreateIncident(_ context.Context, i *domain.Incident) error {
	c := *i
	m.incidents[i.ID] = &c
	return nil
}

func (m *memStore) Incident(_ context.Context, id uuid.UUID) (*domain.Incident, error) {
	i, ok := m.incidents[id]
	if !ok {
		return nil, domain.ErrIncidentNotFound
	}
	c := *i
	c.Updates = append([]domain.IncidentUpdate{}, i.Updates...)
	return &c, nil
}

func (m *memStore) list(keep func(*domain.Incident) bool) []domain.Incident {
	out := []domain.Incident{}
	for _, i := range m.incidents {
		if keep(i) {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

func (m *memStore) Incidents(_ context.Context, open bool, _ int) ([]domain.Incident, error) {
	return m.list(func(i *domain.Incident) bool { return !open || !i.Resolved() }), nil
}

func (m *memStore) Public(_ context.Context, since time.Time) ([]domain.Incident, error) {
	return m.list(func(i *domain.Incident) bool { return !i.Resolved() || !i.ResolvedAt.Before(since) }), nil
}

func (m *memStore) SaveIncident(_ context.Context, i *domain.Incident) error {
	c := *i
	c.Updates = m.incidents[i.ID].Updates
	m.incidents[i.ID] = &c
	return nil
}

func (m *memStore) AddUpdate(_ context.Context, i *domain.Incident, u *domain.IncidentUpdate) error {
	x := m.incidents[i.ID]
	x.Status, x.ResolvedAt, x.UpdatedAt = i.Status, i.ResolvedAt, i.UpdatedAt
	x.Updates = append(x.Updates, *u)
	return nil
}

type fakeProbe struct {
	check domain.Check
	err   error
	calls int
}

func (f *fakeProbe) Check(context.Context, time.Time) (domain.Check, error) {
	f.calls++
	return f.check, f.err
}

func TestPublicStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 5, 10, 12, 0, 0, 0, time.UTC)
	vps := &fakeProbe{check: domain.Check{State: domain.StateOperational}}
	domains := &fakeProbe{err: errors.New("relation domain_orders does not exist")}
	s := &Service{Store: &memStore{incidents: map[uuid.UUID]*domain.Incident{}}, Now: func() time.Time { return now },
		Components: []Component{
			{Key: domain.ComponentAPI, Name: domain.NameAPI, Probe: Operational},
			{Key: domain.ComponentVPS, Name: domain.NameVPS, Probe: vps},
			{Key: domain.ComponentDomains, Name: domain.NameDomains, Probe: domains},
		}}

	p, err := s.Public(ctx, i18n.EN)
	require.NoError(t, err)
	assert.Equal(t, domain.StateOperational, p.Overall)
	assert.Equal(t, []PublicComponent{
		{Key: "api", Name: "Console and API", State: domain.StateOperational},
		{Key: "vps", Name: "VPS servers: ordering and management", State: domain.StateOperational},
		{Key: "domains", Name: "Domain names", State: domain.StateUnknown},
	}, p.Components)
	raw, _ := json.Marshal(p)
	assert.NotContains(t, string(raw), "domain_orders", "jamais de détail interne")

	// Cache de 30 s : la sonde n'est pas rappelée.
	vps.check = domain.Check{State: domain.StateDegraded, Reason: domain.ReasonReconcileStale}
	p, _ = s.Public(ctx, i18n.FR)
	assert.Equal(t, domain.StateOperational, p.Components[1].State)
	assert.Equal(t, "Console et API", p.Components[0].Name)
	assert.Equal(t, 1, vps.calls)
	now = now.Add(31 * time.Second)
	p, _ = s.Public(ctx, i18n.FR)
	assert.Equal(t, domain.StateDegraded, p.Components[1].State)
	assert.Equal(t, domain.StateDegraded, p.Overall)
	raw, _ = json.Marshal(p)
	assert.NotContains(t, string(raw), domain.ReasonReconcileStale, "la raison reste pour l'équipe")

	admin, err := s.Admin(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.ReasonReconcileStale, admin.Components[1].Reason)
	assert.Equal(t, domain.ReasonProbeFailed, admin.Components[2].Reason)

	// Incident majeur sur le VPS : panne, même si la sonde dit dégradé.
	inc, err := s.Create(ctx, domain.IncidentInput{TitleFR: "Création de VPS en panne", TitleEN: "VPS creation down",
		MessageFR: "Nous enquêtons.", MessageEN: "We are investigating.", Components: []string{"vps"}, Impact: domain.ImpactMajor}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusInvestigating, inc.Status)
	require.Len(t, inc.Updates, 1)
	p, _ = s.Public(ctx, i18n.EN) // le cache est vidé par la création
	assert.Equal(t, domain.StateOutage, p.Components[1].State)
	assert.Equal(t, domain.StateOutage, p.Overall)
	require.Len(t, p.Incidents, 1)
	assert.Equal(t, "VPS creation down", p.Incidents[0].Title)
	raw, _ = json.Marshal(p)
	assert.NotContains(t, string(raw), "user:staff", "auteur non publié")

	// Un incident mineur n'améliore pas un état pire.
	_, err = s.Create(ctx, domain.IncidentInput{TitleFR: "Lenteurs", TitleEN: "Slowness", MessageFR: "m", MessageEN: "m",
		Components: []string{"vps", "api"}, Impact: domain.ImpactMinor}, "user:staff")
	require.NoError(t, err)
	p, _ = s.Public(ctx, i18n.EN)
	assert.Equal(t, domain.StateOutage, p.Components[1].State)
	assert.Equal(t, domain.StateDegraded, p.Components[0].State)

	// Fil : surveillance, puis résolution.
	inc, err = s.AddUpdate(ctx, inc.ID, domain.UpdateInput{Status: domain.StatusMonitoring, MessageFR: "Corrigé, sous surveillance.",
		MessageEN: "Fixed, monitoring."}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusMonitoring, inc.Status)
	inc, err = s.Resolve(ctx, inc.ID, ResolveInput{}, "user:staff")
	require.NoError(t, err)
	assert.True(t, inc.Resolved())
	require.NotNil(t, inc.ResolvedAt)
	assert.Equal(t, "Incident resolved.", inc.Updates[2].MessageEN)
	_, err = s.AddUpdate(ctx, inc.ID, domain.UpdateInput{Status: domain.StatusMonitoring, MessageFR: "a", MessageEN: "b"}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrIncidentResolved)
	p, _ = s.Public(ctx, i18n.EN)
	assert.Equal(t, domain.StateDegraded, p.Components[1].State, "résolu : la sonde et l'incident mineur restent")
	assert.Len(t, p.Incidents, 2, "résolu depuis moins de 7 jours : encore affiché")
	for _, in := range p.Incidents {
		if in.ID == inc.ID {
			assert.Equal(t, domain.StatusResolved, in.Updates[0].Status, "plus récent d'abord")
		}
	}

	// Maintenance programmée : à venir, sans effet sur l'état.
	starts := now.Add(24 * time.Hour)
	ends := starts.Add(time.Hour)
	m, err := s.Create(ctx, domain.IncidentInput{TitleFR: "Maintenance", TitleEN: "Maintenance", MessageFR: "Mise à jour.",
		MessageEN: "Upgrade.", Components: []string{"domains"}, Impact: domain.ImpactMaintenance, StartsAt: &starts, EndsAt: &ends}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusIdentified, m.Status)
	assert.Equal(t, starts, m.StartedAt)
	p, _ = s.Public(ctx, i18n.EN)
	require.Len(t, p.Maintenances, 1)
	assert.Len(t, p.Incidents, 2)
	assert.Equal(t, domain.StateUnknown, p.Components[2].State)

	// Pendant la fenêtre : dégradé.
	now = starts.Add(10 * time.Minute)
	p, _ = s.Public(ctx, i18n.EN)
	assert.Empty(t, p.Maintenances)
	assert.Equal(t, domain.StateDegraded, p.Components[2].State)

	// Huit jours après la résolution : l'incident n'est plus public.
	now = inc.ResolvedAt.Add(8 * 24 * time.Hour)
	s.invalidate()
	p, _ = s.Public(ctx, i18n.EN)
	for _, in := range p.Incidents {
		assert.NotEqual(t, inc.ID, in.ID)
	}

	// Modifier : textes et composants, le fil ne change pas.
	edited, err := s.Edit(ctx, m.ID, domain.IncidentInput{TitleFR: "Maintenance DNS", TitleEN: "DNS maintenance", MessageFR: "Mise à jour.",
		MessageEN: "Upgrade.", Components: []string{"domains", "api"}, Impact: domain.ImpactMaintenance, StartsAt: &starts, EndsAt: &ends}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "DNS maintenance", edited.TitleEN)
	assert.Len(t, edited.Updates, 1)
	_, err = s.Edit(ctx, m.ID, domain.IncidentInput{TitleFR: "x", TitleEN: "x", MessageFR: "x", MessageEN: "x", Components: []string{"nope"},
		Impact: domain.ImpactMinor}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrInvalidComponents)
	_, err = s.Get(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrIncidentNotFound)
}

func TestDefaultComponents(t *testing.T) {
	cs := DefaultComponents(nil, Features{VPS: true, Domains: true})
	require.Len(t, cs, 3)
	assert.Equal(t, []string{"api", "vps", "domains"}, []string{cs[0].Key, cs[1].Key, cs[2].Key})
	assert.NotNil(t, cs[0].Probe)
	assert.Nil(t, cs[1].Probe, "sans signaux : unknown")
	for _, c := range cs {
		assert.True(t, i18n.Known(c.Name), "%s : nom absent du catalogue", c.Key)
		assert.NotEqual(t, c.Name, i18n.Translate(i18n.EN, c.Name))
	}
}
