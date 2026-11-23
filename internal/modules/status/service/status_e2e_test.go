package service_test

// Incidents et sonde VPS sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/status/... -count=1
//
// IAM_E2E_DSN remplace la base par défaut (127.0.0.1:55433).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
	"github.com/thekrauss/kubemanager/internal/modules/status/repository"
	"github.com/thekrauss/kubemanager/internal/modules/status/service"
)

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	dsn := os.Getenv("IAM_E2E_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// TestE2E_StatusProbes : tout se passe dans une transaction annulée à la fin
// (les tables de compute et de domaines restent intactes).
func TestE2E_StatusProbes(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	tx := db.Begin()
	defer tx.Rollback()
	for _, table := range []string{"compute_reconcile_runs", "compute_requests", "domain_orders"} {
		require.NoError(t, tx.Exec(`DELETE FROM `+table).Error)
	}
	store := &repository.GormStore{DB: tx}
	now := time.Now().UTC()
	vps := service.VPSProbe(store)
	domains := service.DomainsProbe(store)
	check := func(p service.Probe) domain.Check {
		c, err := p.Check(ctx, now)
		require.NoError(t, err)
		return c
	}

	assert.Equal(t, domain.Check{State: domain.StateUnknown, Reason: domain.ReasonNoReconcile}, check(vps))
	assert.Equal(t, domain.StateOperational, check(domains).State)

	run := func(started, finished time.Time, errText string) {
		require.NoError(t, tx.Exec(`INSERT INTO compute_reconcile_runs (id, provider, started_at, finished_at, error) VALUES (?, 'p', ?, ?, ?)`,
			uuid.New(), started, finished, errText).Error)
	}
	run(now.Add(-50*time.Minute), now.Add(-49*time.Minute), "")
	assert.Equal(t, domain.Check{State: domain.StateDegraded, Reason: domain.ReasonReconcileStale}, check(vps))
	run(now.Add(-6*time.Minute), now.Add(-5*time.Minute), "provider: 503")
	assert.Equal(t, domain.Check{State: domain.StateDegraded, Reason: domain.ReasonReconcileFailed}, check(vps))
	run(now.Add(-2*time.Minute), now.Add(-time.Minute), "")
	assert.Equal(t, domain.Check{State: domain.StateOperational}, check(vps))
	// Passe en cours (pas finie) : la précédente compte.
	require.NoError(t, tx.Exec(`INSERT INTO compute_reconcile_runs (id, provider, started_at) VALUES (?, 'p', ?)`, uuid.New(), now).Error)
	assert.Equal(t, domain.StateOperational, check(vps).State)

	req := func(status string, created time.Time, started *time.Time) {
		require.NoError(t, tx.Exec(`INSERT INTO compute_requests (id, organization_id, kind, status, created_at, started_at) VALUES (?, ?, 'ping', ?, ?, ?)`,
			uuid.New(), uuid.New(), status, created, started).Error)
	}
	req("pending", now.Add(-5*time.Minute), nil)
	req("done", now.Add(-48*time.Hour), nil)
	assert.Equal(t, domain.StateOperational, check(vps).State)
	old := now.Add(-7 * time.Hour)
	req("started", old, &old)
	assert.Equal(t, domain.Check{State: domain.StateDegraded, Reason: domain.ReasonRequestsStarted}, check(vps))
	req("pending", now.Add(-20*time.Minute), nil)
	assert.Equal(t, domain.Check{State: domain.StateDegraded, Reason: domain.ReasonRequestsPending}, check(vps))

	require.NoError(t, tx.Exec(`INSERT INTO domain_orders (id, organization_id, domain, status, currency, price, renew_price, contact, updated_at)
		VALUES (?, ?, ?, 'ordering', 'XOF', 1, 1, '{}', ?)`, uuid.New(), uuid.New(), "e2e-"+uuid.NewString()[:8]+".sn", now.Add(-2*time.Hour)).Error)
	assert.Equal(t, domain.Check{State: domain.StateDegraded, Reason: domain.ReasonDomainsOrdering}, check(domains))
}

func TestE2E_StatusIncidents(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	tx := db.Begin()
	defer tx.Rollback()
	require.NoError(t, tx.Exec(`DELETE FROM status_incidents`).Error)
	store := &repository.GormStore{DB: tx}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := &service.Service{Store: store, Now: func() time.Time { return now },
		Components: service.DefaultComponents(nil, service.Features{})}

	inc, err := s.Create(ctx, domain.IncidentInput{TitleFR: "Commandes de VPS en attente", TitleEN: "VPS orders delayed",
		MessageFR: "Nous enquêtons.", MessageEN: "We are investigating.", Components: []string{"vps"}, Impact: domain.ImpactMinor}, "user:staff")
	require.NoError(t, err)
	got, err := s.Get(ctx, inc.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"vps"}, got.Components)
	require.Len(t, got.Updates, 1)
	assert.Equal(t, domain.StatusInvestigating, got.Updates[0].Status)

	now = now.Add(time.Minute)
	_, err = s.AddUpdate(ctx, inc.ID, domain.UpdateInput{Status: domain.StatusIdentified, MessageFR: "Cause trouvée.", MessageEN: "Cause found."}, "user:staff")
	require.NoError(t, err)
	p, err := s.Public(ctx, i18n.FR)
	require.NoError(t, err)
	assert.Equal(t, domain.StateDegraded, p.Components[1].State)
	require.Len(t, p.Incidents, 1)
	assert.Equal(t, "Cause trouvée.", p.Incidents[0].Updates[0].Message, "plus récent d'abord")

	open, err := s.List(ctx, true, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)

	edited, err := s.Edit(ctx, inc.ID, domain.IncidentInput{TitleFR: "Commandes de VPS ralenties", TitleEN: "VPS orders slowed down",
		MessageFR: "Nous enquêtons.", MessageEN: "We are investigating.", Components: []string{"vps", "api"}, Impact: domain.ImpactMajor}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusIdentified, edited.Status, "le statut ne change que par un message")
	assert.Len(t, edited.Updates, 2)
	p, _ = s.Public(ctx, i18n.EN)
	assert.Equal(t, domain.StateOutage, p.Overall)

	now = now.Add(time.Minute)
	resolved, err := s.Resolve(ctx, inc.ID, service.ResolveInput{}, "user:staff")
	require.NoError(t, err)
	assert.True(t, resolved.Resolved())
	require.NotNil(t, resolved.ResolvedAt)
	require.Len(t, resolved.Updates, 3)
	assert.Equal(t, "Incident résolu.", resolved.Updates[2].MessageFR)
	_, err = s.AddUpdate(ctx, inc.ID, domain.UpdateInput{Status: domain.StatusMonitoring, MessageFR: "a", MessageEN: "b"}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrIncidentResolved)
	open, _ = s.List(ctx, true, 0)
	assert.Empty(t, open)
	p, _ = s.Public(ctx, i18n.EN)
	assert.Len(t, p.Incidents, 1, "résolu depuis moins de 7 jours")
	assert.Equal(t, domain.StateOperational, p.Components[0].State)

	starts, ends := now.Add(48*time.Hour), now.Add(50*time.Hour)
	_, err = s.Create(ctx, domain.IncidentInput{TitleFR: "Maintenance", TitleEN: "Maintenance", MessageFR: "Mise à jour du réseau.",
		MessageEN: "Network upgrade.", Components: []string{"vps"}, Impact: domain.ImpactMaintenance, StartsAt: &starts, EndsAt: &ends}, "user:staff")
	require.NoError(t, err)
	p, _ = s.Public(ctx, i18n.EN)
	require.Len(t, p.Maintenances, 1)
	assert.Equal(t, "Network upgrade.", p.Maintenances[0].Message)

	_, err = s.Get(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrIncidentNotFound)
}
