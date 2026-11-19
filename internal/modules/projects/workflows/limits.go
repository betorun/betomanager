package workflows

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// UpdateProjectLimitsInput : nouvelles limites à reporter sur le quota
// Kubernetes de l'espace du projet.
type UpdateProjectLimitsInput struct {
	ProjectID   string `json:"project_id"`
	Namespace   string `json:"namespace"`
	CpuLimit    string `json:"cpu_limit"`
	MemoryLimit string `json:"memory_limit"`
}

// UpdateProjectLimitsWorkflow : ajuste le quota de l'espace du projet
// (activité idempotente, partagée avec la création du projet).
func UpdateProjectLimitsWorkflow(ctx workflow.Context, input UpdateProjectLimitsInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: activityStartToCloseTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: activityRetryInitialInterval,
			MaximumAttempts: activityRetryMaximumAttempts,
		},
	})
	var a *ProjectActivities
	return workflow.ExecuteActivity(ctx, a.ReconcileProjectResources, input.Namespace, input.CpuLimit, input.MemoryLimit).Get(ctx, nil)
}
