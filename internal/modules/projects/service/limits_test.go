package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"

	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/workflows"
	workloadRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
)

// usageRepo : consommation réservée par les conteneurs du projet (milli-CPU, Mi).
type usageRepo struct {
	workloadRepo.WorkloadRepository
	cpu, memMi int64
}

func (u usageRepo) GetTotalUsageByProject(context.Context, uuid.UUID) (int64, int64, int64, error) {
	return u.cpu, u.memMi, 0, nil
}

func limitsFixture(dist *fakeDistributor, usedCPU, usedMemMi int64) (*ProjectService, *fakeProjectRepo) {
	repo := &fakeProjectRepo{project: &authdomain.Project{
		ID: uuid.New(), Name: "bomba", CpuLimit: "2000m", MemoryLimit: "4Gi", StorageLimit: "10Gi",
	}}
	svc := newProjectServiceForTest(dist, repo)
	svc.WorkloadRepo = usageRepo{cpu: usedCPU, memMi: usedMemMi}
	return svc, repo
}

// Champ vide : valeur actuelle gardée ; base mise à jour puis quota ajusté
// par le workflow, sur l'espace du projet.
func TestSetLimits_UpdatesRowThenStartsWorkflow(t *testing.T) {
	dist := &fakeDistributor{handle: temporal.WorkflowHandle{WorkflowID: "wf-limits"}}
	svc, repo := limitsFixture(dist, 1500, 2048)
	id := repo.project.ID.String()

	resp, err := svc.SetLimits(context.Background(), id, typesproject.ProjectLimits{CPU: "4000m"})

	require.NoError(t, err)
	assert.Equal(t, typesproject.UsageDTO{CPU: "4000m", Memory: "4Gi", Storage: "10Gi"}, resp.Limits)
	assert.Equal(t, "wf-limits", resp.WorkflowID)
	require.Len(t, repo.limits, 1)
	assert.Equal(t, [4]string{id, "4000m", "4Gi", "10Gi"}, repo.limits[0])
	assert.Equal(t, workflows.WorkflowIDLimitsPrefix+id, dist.gotOpts.WorkflowID)
	in := dist.gotArgs[0].(workflows.UpdateProjectLimitsInput)
	assert.Equal(t, "km-bomba", in.Namespace)
	assert.Equal(t, "4000m", in.CpuLimit)
	assert.Equal(t, "4Gi", in.MemoryLimit)
}

func TestSetLimits_RejectsInvalidQuantities(t *testing.T) {
	for _, in := range []typesproject.ProjectLimits{{CPU: "beaucoup"}, {Memory: "0"}, {Storage: "-1Gi"}} {
		svc, repo := limitsFixture(&fakeDistributor{}, 0, 0)
		_, err := svc.SetLimits(context.Background(), repo.project.ID.String(), in)
		assert.ErrorIs(t, err, ErrInvalidProjectLimits, "%+v", in)
		assert.Empty(t, repo.limits, "rien d'écrit")
	}
}

// Sous ce que les conteneurs réservent déjà : refusé, rien n'est écrit.
func TestSetLimits_RejectsBelowUsage(t *testing.T) {
	cases := []typesproject.ProjectLimits{{CPU: "1000m"}, {Memory: "1Gi"}}
	for _, in := range cases {
		dist := &fakeDistributor{}
		svc, repo := limitsFixture(dist, 1500, 2048)
		_, err := svc.SetLimits(context.Background(), repo.project.ID.String(), in)
		assert.ErrorIs(t, err, ErrLimitsBelowUsage, "%+v", in)
		assert.Empty(t, repo.limits)
		assert.Nil(t, dist.gotWorkflow, "aucun workflow")
	}
}

func TestSetLimits_UpdateAlreadyRunning(t *testing.T) {
	dist := &fakeDistributor{err: fmt.Errorf("execute workflow: %w", serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "run"))}
	svc, repo := limitsFixture(dist, 0, 0)
	_, err := svc.SetLimits(context.Background(), repo.project.ID.String(), typesproject.ProjectLimits{CPU: "3000m"})
	assert.ErrorIs(t, err, ErrLimitsUpdateInProgress)
}
