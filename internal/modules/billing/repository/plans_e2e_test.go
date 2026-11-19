package repository_test

// Plans sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/billing/... -count=1 -v

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

// fakeLimiter : limites appliquées aux projets ; tooBig : projets qui
// réservent déjà plus (refus comme le service des projets).
type fakeLimiter struct {
	set    map[uuid.UUID]string
	tooBig map[uuid.UUID]bool
}

type belowUsageErr struct{}

func (belowUsageErr) Error() string    { return "below usage" }
func (belowUsageErr) BelowUsage() bool { return true }

func (f *fakeLimiter) SetLimits(_ context.Context, project uuid.UUID, cpu, memory, storage string) error {
	if f.tooBig[project] {
		return belowUsageErr{}
	}
	f.set[project] = cpu + "/" + memory + "/" + storage
	return nil
}

func TestE2E_Plans(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	now := time.Date(2030, 8, 10, 12, 10, 0, 0, time.UTC)
	s := walletService(db, &now, &fakePayments{status: "pending"})
	s.Plans = repository.NewPlans(db)
	limiter := &fakeLimiter{set: map[uuid.UUID]string{}, tooBig: map[uuid.UUID]bool{}}
	s.Limits = limiter

	pub, err := s.PublicPlans(ctx, "xaf")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(pub.Plans), 3)
	assert.Equal(t, "free", pub.Plans[0].ID)

	cur, err := s.OrgPlan(ctx, org)
	require.NoError(t, err)
	assert.Equal(t, "free", cur.Current.ID, "sans compte : plan gratuit")

	// Plan payant sans facturation active : refusé au client, permis à l'équipe.
	_, err = s.ChangePlan(ctx, org, "pro", "user:client", false)
	assert.ErrorIs(t, err, domain.ErrPlanNeedsBilling)
	_, err = s.SetCharging(ctx, org, true, "e2e")
	require.NoError(t, err)
	got, err := s.ChangePlan(ctx, org, "pro", "user:client", false)
	require.NoError(t, err)
	assert.Equal(t, "pro", got.Current.ID)
	assert.Equal(t, "8000m/16Gi/160Gi", limiter.set[project], "les limites du projet suivent le plan")

	// L'abonnement est mesuré au relevé, rattaché à l'organisation.
	_, err = s.Sample(ctx)
	require.NoError(t, err)
	var plans []domain.Usage
	require.NoError(t, db.Where("organization_id = ? AND meter = ?", org, domain.MeterPlan).Find(&plans).Error)
	require.Len(t, plans, 1)
	assert.Equal(t, uuid.Nil, plans[0].ProjectID)
	assert.Equal(t, domain.PerHour(domain.FromUnits(15000)), plans[0].Amount)

	// Descente refusée si un projet réserve déjà plus que le nouveau plan.
	require.NoError(t, db.Exec(`INSERT INTO workloads (project_id, name, namespace, chart_name, version, image, status, replicas, cpu_limit, memory_limit)
		VALUES (?, 'gros', 'ns', 'standard-app', '1', 'img', 'RUNNING', 3, '1000m', '2Gi')`, project).Error)
	limiter.set = map[uuid.UUID]string{}
	_, err = s.ChangePlan(ctx, org, "free", "user:client", false)
	var below *domain.PlanBelowUsageError
	require.ErrorAs(t, err, &below)
	assert.Len(t, below.Projects, 1)
	assert.Empty(t, limiter.set, "rien n'est appliqué à moitié")
	cur, _ = s.OrgPlan(ctx, org)
	assert.Equal(t, "pro", cur.Current.ID)

	// Un nouveau projet de l'organisation reçoit les limites du plan au
	// rapprochement suivant.
	second := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, organization_id) VALUES (?, ?, ?)`, second, "e2e-second-"+second.String()[:8], org).Error)
	limiter.set = map[uuid.UUID]string{}
	_, missed, err := s.ReconcileLimits(ctx, &org, nil)
	require.NoError(t, err)
	assert.Empty(t, missed)
	assert.Equal(t, "8000m/16Gi/160Gi", limiter.set[second])

	// Plan modifié : ses limites s'appliquent aussitôt aux projets des
	// organisations qui l'ont ; un projet qui réserve déjà plus est signalé.
	limiter.set = map[uuid.UUID]string{}
	limiter.tooBig[project] = true
	pro := pub.Plans[2]
	require.Equal(t, "pro", pro.ID)
	saved, err := s.SavePlan(ctx, domain.PlanUpdate{ID: "pro", Name: pro.Name, Position: 2, Public: true, CPULimit: "6000m",
		MemoryLimit: "12Gi", StorageLimit: "120Gi", Fees: map[string]int64{"XAF": 15000, "XOF": 15000}}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, "6000m/12Gi/120Gi", limiter.set[second])
	require.Len(t, saved.NotApplied, 1)
	assert.Equal(t, project.String(), saved.NotApplied[0].ProjectID)
	limiter.tooBig = map[uuid.UUID]bool{}

	// Exception de l'équipe : le rapprochement la respecte, puis elle se retire.
	require.NoError(t, s.RecordOverride(ctx, second.String(), "1000m", "1Gi", "5Gi", "user:staff"))
	limiter.set = map[uuid.UUID]string{}
	_, _, err = s.ReconcileLimits(ctx, &org, nil)
	require.NoError(t, err)
	assert.Equal(t, "1000m/1Gi/5Gi", limiter.set[second], "l'exception, pas le plan")
	limiter.set = map[uuid.UUID]string{}
	_, err = s.ClearOverride(ctx, second)
	require.NoError(t, err)
	assert.Equal(t, "6000m/12Gi/120Gi", limiter.set[second], "de retour au plan")

	// L'équipe modifie un plan ; un plan non public n'est pas proposé.
	_, err = s.SavePlan(ctx, domain.PlanUpdate{ID: "partenaire-" + uuid.NewString()[:6], Name: "Partenaire", Position: 9, CPULimit: "16000m",
		MemoryLimit: "32Gi", StorageLimit: "500Gi", Fees: map[string]int64{"XAF": 0}}, "user:staff")
	require.NoError(t, err)
	pub2, err := s.PublicPlans(ctx, "XAF")
	require.NoError(t, err)
	assert.Equal(t, len(pub.Plans), len(pub2.Plans), "plan non public : pas dans l'offre")
	_, err = s.ChangePlan(ctx, org, "inconnu", "user:client", false)
	assert.ErrorIs(t, err, domain.ErrPlanNotFound)
}
