package repository_test

// Suppression des données après une longue suspension, sur un vrai Postgres.
// Ignoré sans IAM_E2E (voir phase4_e2e_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

type fakePurger struct {
	calls []uuid.UUID
	until []time.Time
	err   error
}

func (f *fakePurger) PurgeOrganization(_ context.Context, org uuid.UUID, until time.Time) error {
	f.calls = append(f.calls, org)
	f.until = append(f.until, until)
	return f.err
}

type notice struct {
	stage int
	at    time.Time
}

type fakePurgeAlerts struct{ sent []notice }

func (f *fakePurgeAlerts) PurgeNotice(_ context.Context, _ uuid.UUID, stage int, at time.Time) {
	f.sent = append(f.sent, notice{stage, at})
}

func TestE2E_SuspensionPurge(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	suspendedAt := time.Date(2030, 5, 1, 9, 0, 0, 0, time.UTC)
	now := suspendedAt
	s := walletService(db, &now, &fakePayments{status: "pending"})
	purger, alerts := &fakePurger{}, &fakePurgeAlerts{}
	s.Purges, s.Purger, s.PurgeAlerts = repository.NewPurges(db), purger, alerts

	_, err := s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	require.NoError(t, s.Phase4.SetSuspended(ctx, org, &suspendedAt))

	// L'edge voit le projet suspendu (vue sans montant).
	var suspended []uuid.UUID
	require.NoError(t, db.Raw(`SELECT project_id FROM edge_suspended_projects WHERE project_id = ?`, project).Scan(&suspended).Error)
	assert.Equal(t, []uuid.UUID{project}, suspended)

	purgeAt := suspendedAt.AddDate(0, 0, 30)
	w, err := s.Wallet(ctx, org)
	require.NoError(t, err)
	require.NotNil(t, w.PurgeAt, "la console annonce la date de suppression")
	assert.True(t, purgeAt.Equal(*w.PurgeAt))
	step := func(at time.Time) { now = at; s.PurgeSuspended(ctx) }

	step(suspendedAt.Add(6 * 24 * time.Hour))
	assert.Empty(t, alerts.sent, "rien avant J+7")
	step(suspendedAt.Add(7 * 24 * time.Hour))
	step(suspendedAt.Add(8 * 24 * time.Hour))
	require.Len(t, alerts.sent, 1, "un seul premier avertissement")
	assert.Equal(t, domain.PurgeStageFirst, alerts.sent[0].stage)
	assert.True(t, purgeAt.Equal(alerts.sent[0].at), "date de suppression annoncée")
	step(purgeAt.Add(-7 * 24 * time.Hour))
	step(purgeAt.Add(-2 * 24 * time.Hour))
	require.Len(t, alerts.sent, 3)
	assert.Equal(t, domain.PurgeStageWeek, alerts.sent[1].stage)
	assert.Equal(t, domain.PurgeStageLast, alerts.sent[2].stage)
	assert.Empty(t, purger.calls)

	// Échec partiel : repris au passage suivant, pas d'e-mail « supprimé ».
	purger.err = assert.AnError
	step(purgeAt)
	require.Len(t, purger.calls, 1)
	assert.Len(t, alerts.sent, 3)
	purger.err = nil
	step(purgeAt.Add(time.Hour))
	require.Len(t, purger.calls, 2)
	assert.True(t, purgeAt.Add(time.Hour).AddDate(0, 0, 30).Equal(purger.until[1]), "copies finales gardées 30 jours")
	require.Len(t, alerts.sent, 4)
	assert.Equal(t, domain.PurgeStageDue, alerts.sent[3].stage)
	w, err = s.Wallet(ctx, org)
	require.NoError(t, err)
	require.NotNil(t, w.DataPurgedAt, "la console sait que les données sont supprimées")
	step(purgeAt.Add(2 * time.Hour))
	assert.Len(t, purger.calls, 2, "une organisation purgée ne l'est plus")

	// Reprise : le compte à rebours repart de zéro, l'edge ne la voit plus suspendue.
	require.NoError(t, s.Phase4.SetSuspended(ctx, org, nil))
	stage, purgedAt, err := s.Purges.PurgeState(ctx, org)
	require.NoError(t, err)
	assert.Zero(t, stage)
	assert.Nil(t, purgedAt)
	suspended = nil
	require.NoError(t, db.Raw(`SELECT project_id FROM edge_suspended_projects WHERE project_id = ?`, project).Scan(&suspended).Error)
	assert.Empty(t, suspended)
}

// Avertissements manqués (processus arrêté) : jamais de suppression sans le
// dernier avertissement envoyé depuis deux jours.
func TestE2E_SuspensionPurgeNeverWithoutLastNotice(t *testing.T) {
	db, org, _ := e2e(t)
	ctx := context.Background()
	suspendedAt := time.Date(2030, 6, 1, 9, 0, 0, 0, time.UTC)
	now := suspendedAt.AddDate(0, 0, 40)
	s := walletService(db, &now, &fakePayments{status: "pending"})
	purger, alerts := &fakePurger{}, &fakePurgeAlerts{}
	s.Purges, s.Purger, s.PurgeAlerts = repository.NewPurges(db), purger, alerts
	_, err := s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	require.NoError(t, s.Phase4.SetSuspended(ctx, org, &suspendedAt))

	s.PurgeSuspended(ctx)
	require.Len(t, alerts.sent, 1)
	assert.Equal(t, domain.PurgeStageLast, alerts.sent[0].stage)
	assert.True(t, now.Add(domain.PurgeLastNotice).Equal(alerts.sent[0].at))
	now = now.Add(47 * time.Hour)
	s.PurgeSuspended(ctx)
	assert.Empty(t, purger.calls, "deux jours pleins après le dernier avertissement")
	now = now.Add(time.Hour)
	s.PurgeSuspended(ctx)
	assert.Len(t, purger.calls, 1)
}
