// Package workflows : workflows Temporal des VPS, exécutés par le mode
// compute (APP_MODE=compute, file de tâches « compute »), seul process qui
// charge les clés des fournisseurs (docs/modules/compute.md).
package workflows

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// Noms et file.
const (
	DefaultTaskQueue    = "compute"
	RequestWorkflowName = "ComputeRequestWorkflow"
)

// Types d'opération. compute.ping : traverse toute la chaîne (outbox, relais,
// Temporal, worker compute, base) sans toucher à un fournisseur.
const KindPing = "compute.ping"

// RequestWorkflow : une demande de l'outbox. Chaque étape est une activité ;
// la demande est close en base à la fin (réussie ou non).
func RequestWorkflow(ctx workflow.Context, in service.RequestInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 2 * time.Second, BackoffCoefficient: 2, MaximumAttempts: 10},
	})
	var a *Activities
	var runErr error
	switch in.Kind {
	case KindPing:
		runErr = workflow.ExecuteActivity(ctx, a.Ping, in.RequestID).Get(ctx, nil)
	default:
		runErr = temporal.NewNonRetryableApplicationError(fmt.Sprintf("unknown compute request kind %q", in.Kind), "UnknownKind", nil)
	}
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	if err := workflow.ExecuteActivity(ctx, a.Finish, in.RequestID, runErr == nil, msg).Get(ctx, nil); err != nil {
		return err
	}
	return runErr
}

// Activities : accès à la base et au fournisseur (mode compute seulement).
type Activities struct {
	Store    service.Store
	Provider service.ComputeProvider // nil : aucun fournisseur configuré
	Now      func() time.Time
}

func (a *Activities) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// Ping : la demande existe en base (le worker compute la lit).
func (a *Activities) Ping(ctx context.Context, requestID string) error {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid request id", "InvalidRequest", err)
	}
	r, err := a.Store.Request(ctx, id)
	if err != nil {
		return err
	}
	if r == nil {
		return temporal.NewNonRetryableApplicationError("compute request not found", "RequestNotFound", nil)
	}
	return nil
}

// Finish : clôt la demande (idempotent).
func (a *Activities) Finish(ctx context.Context, requestID string, ok bool, msg string) error {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid request id", "InvalidRequest", err)
	}
	return a.Store.FinishRequest(ctx, id, ok, msg, a.now())
}
