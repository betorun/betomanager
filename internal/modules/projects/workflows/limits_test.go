package workflows

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func TestUpdateProjectLimitsWorkflow_ReconcilesQuota(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	f := &fakeProjectActs{}
	registerFakeProjectActs(env, f)

	env.ExecuteWorkflow(UpdateProjectLimitsWorkflow, UpdateProjectLimitsInput{
		ProjectID: "p1", Namespace: "km-bomba", CpuLimit: "4000m", MemoryLimit: "8Gi",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, "km-bomba", f.reconcileNS)
	assert.Equal(t, "4000m", f.reconcileCPU)
	assert.Equal(t, "8Gi", f.reconcileMem)
}

func TestUpdateProjectLimitsWorkflow_SurfacesFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	f := &fakeProjectActs{reconcileErr: errors.New("api server down")}
	registerFakeProjectActs(env, f)

	env.ExecuteWorkflow(UpdateProjectLimitsWorkflow, UpdateProjectLimitsInput{Namespace: "km-bomba", CpuLimit: "1", MemoryLimit: "1Gi"})

	require.True(t, env.IsWorkflowCompleted())
	assert.Error(t, env.GetWorkflowError())
}
