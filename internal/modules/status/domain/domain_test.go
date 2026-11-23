package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var now = time.Date(2030, 5, 10, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time {
	t := now.Add(-d)
	return &t
}

func TestVPSCheck(t *testing.T) {
	cases := []struct {
		name string
		s    VPSSignals
		want Check
	}{
		{"jamais de réconciliation", VPSSignals{}, Check{StateUnknown, ReasonNoReconcile}},
		{"passe récente sans erreur", VPSSignals{LastFinished: ago(5 * time.Minute), FirstStarted: ago(48 * time.Hour)}, Check{State: StateOperational}},
		{"passe récente en erreur", VPSSignals{LastFinished: ago(5 * time.Minute), LastFailed: true, FirstStarted: ago(time.Hour)}, Check{StateDegraded, ReasonReconcileFailed}},
		{"dernière passe trop ancienne", VPSSignals{LastFinished: ago(31 * time.Minute), FirstStarted: ago(time.Hour)}, Check{StateDegraded, ReasonReconcileStale}},
		{"première passe en cours", VPSSignals{FirstStarted: ago(10 * time.Minute)}, Check{State: StateOperational}},
		{"première passe jamais finie", VPSSignals{FirstStarted: ago(40 * time.Minute)}, Check{StateDegraded, ReasonReconcileStale}},
		{"demande en attente récente", VPSSignals{LastFinished: ago(time.Minute), FirstStarted: ago(time.Hour), OldestPending: ago(14 * time.Minute)}, Check{State: StateOperational}},
		{"demande en attente bloquée", VPSSignals{LastFinished: ago(time.Minute), FirstStarted: ago(time.Hour), OldestPending: ago(16 * time.Minute)}, Check{StateDegraded, ReasonRequestsPending}},
		{"demande lancée bloquée", VPSSignals{LastFinished: ago(time.Minute), FirstStarted: ago(time.Hour), OldestStarted: ago(7 * time.Hour)}, Check{StateDegraded, ReasonRequestsStarted}},
		{"demande lancée depuis 5 h", VPSSignals{LastFinished: ago(time.Minute), FirstStarted: ago(time.Hour), OldestStarted: ago(5 * time.Hour)}, Check{State: StateOperational}},
		{"sans réconciliation, demande bloquée", VPSSignals{OldestPending: ago(time.Hour)}, Check{StateDegraded, ReasonRequestsPending}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, VPSCheck(c.s, now), c.name)
	}
}

func TestDomainsCheck(t *testing.T) {
	assert.Equal(t, StateOperational, DomainsCheck(nil, now).State)
	assert.Equal(t, StateOperational, DomainsCheck(ago(59*time.Minute), now).State)
	assert.Equal(t, Check{StateDegraded, ReasonDomainsOrdering}, DomainsCheck(ago(61*time.Minute), now))
}

func TestWorseAndOverall(t *testing.T) {
	assert.Equal(t, StateDegraded, Worse(StateOperational, StateDegraded))
	assert.Equal(t, StateOutage, Worse(StateOutage, StateDegraded))
	assert.Equal(t, StateOperational, Worse(StateOperational, StateUnknown), "unknown n'aggrave pas")
	assert.Equal(t, StateDegraded, Worse(StateUnknown, StateDegraded))
	assert.Equal(t, StateUnknown, Overall([]string{StateUnknown, StateUnknown}))
	assert.Equal(t, StateOperational, Overall([]string{StateOperational, StateUnknown}))
	assert.Equal(t, StateOutage, Overall([]string{StateOperational, StateOutage, StateDegraded}))
}

func TestIncidentActivity(t *testing.T) {
	inc := Incident{Impact: ImpactMajor, Status: StatusInvestigating}
	assert.True(t, inc.Active(now))
	inc.Status = StatusResolved
	assert.False(t, inc.Active(now))

	m := Incident{Impact: ImpactMaintenance, Status: StatusIdentified, StartsAt: ptr(now.Add(time.Hour)), EndsAt: ptr(now.Add(2 * time.Hour))}
	assert.True(t, m.Upcoming(now))
	assert.False(t, m.Active(now))
	assert.True(t, m.Active(now.Add(90*time.Minute)), "pendant la fenêtre")
	assert.False(t, m.Active(now.Add(3*time.Hour)), "après la fenêtre")
	assert.Equal(t, StateDegraded, ImpactState(ImpactMinor))
	assert.Equal(t, StateOutage, ImpactState(ImpactMajor))
	assert.Equal(t, StateDegraded, ImpactState(ImpactMaintenance))
}

func ptr(t time.Time) *time.Time { return &t }

func TestIncidentInputValidate(t *testing.T) {
	known := func(k string) bool { return k == ComponentVPS || k == ComponentAPI }
	ok := IncidentInput{TitleFR: "Panne", TitleEN: "Outage", MessageFR: "Nous cherchons.", MessageEN: "We are looking.",
		Components: []string{" vps ", "vps"}, Impact: ImpactMajor}.Normalize()
	assert.Equal(t, []string{ComponentVPS}, ok.Components)
	assert.NoError(t, ok.Validate(known))

	bad := ok
	bad.TitleEN = ""
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidIncident)
	bad = ok
	bad.Impact = "critical"
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidImpact)
	bad = ok
	bad.Components = []string{"dns"}
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidComponents)
	bad = ok
	bad.Status = "open"
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidStatus)
	bad = ok
	bad.Impact = ImpactMaintenance
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidWindow)
	bad.StartsAt, bad.EndsAt = ptr(now), ptr(now.Add(-time.Minute))
	assert.ErrorIs(t, bad.Validate(known), ErrInvalidWindow)
	bad.EndsAt = ptr(now.Add(time.Hour))
	assert.NoError(t, bad.Validate(known))

	assert.ErrorIs(t, UpdateInput{Status: "done", MessageFR: "a", MessageEN: "b"}.Validate(), ErrInvalidStatus)
	assert.ErrorIs(t, UpdateInput{Status: StatusMonitoring, MessageFR: "a"}.Validate(), ErrInvalidStatusUpdate)
	assert.NoError(t, UpdateInput{Status: StatusMonitoring, MessageFR: "a", MessageEN: "b"}.Validate())
}
