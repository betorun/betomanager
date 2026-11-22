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

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// Noms et file.
const (
	DefaultTaskQueue      = "compute"
	RequestWorkflowName   = "ComputeRequestWorkflow"
	ReconcileWorkflowName = "ComputeReconcileWorkflow"
	// ReconcileWorkflowID : un seul workflow périodique, quel que soit le
	// nombre de process compute.
	ReconcileWorkflowID = "compute-reconcile"
	// ReconcileCron : toutes les 10 minutes.
	ReconcileCron = "*/10 * * * *"
)

// Types d'opération. compute.ping : traverse toute la chaîne (outbox, relais,
// Temporal, worker compute, base) sans toucher à un fournisseur.
// compute.reconcile : une passe de réconciliation demandée par l'équipe.
const (
	KindPing      = "compute.ping"
	KindReconcile = "compute.reconcile"
	KindCreate    = domain.KindCreate
	KindStart     = domain.KindStart
	KindStop      = domain.KindStop
	KindReboot    = domain.KindReboot
	KindReinstall = domain.KindReinstall
)

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
	case KindCreate:
		runErr = createVM(ctx, in.RequestID)
	case KindStart, KindStop:
		runErr = powerVM(ctx, in.RequestID, in.Kind == KindStart)
	case KindReboot:
		runErr = rebootVM(ctx, in.RequestID)
	case KindReinstall:
		runErr = reinstallVM(ctx, in.RequestID)
	case KindReconcile:
		runErr = workflow.ExecuteActivity(reconcileOptions(ctx), a.Reconcile).Get(ctx, nil)
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

func reconcileOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 30 * time.Second, BackoffCoefficient: 2, MaximumAttempts: 3},
	})
}

// ReconcileWorkflow : passe périodique (planifiée par ReconcileCron) ; une
// passe en échec est journalisée en base et retentée à la suivante.
func ReconcileWorkflow(ctx workflow.Context) error {
	var a *Activities
	return workflow.ExecuteActivity(reconcileOptions(ctx), a.Reconcile).Get(ctx, nil)
}

// Activities : accès à la base et au fournisseur (mode compute seulement).
type Activities struct {
	Store    service.Store
	Provider service.ComputeProvider // nil : aucun fournisseur configuré
	Orderer  service.Orderer         // nil : pas de commande possible
	Wallet   service.Wallet          // remboursements
	Events   service.Events          // temps réel (nil : aucun)
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

// Reconcile : une passe base ↔ fournisseur.
func (a *Activities) Reconcile(ctx context.Context) error {
	if a.Provider == nil {
		return temporal.NewNonRetryableApplicationError("no compute provider configured", "NoProvider", nil)
	}
	_, err := (&service.Reconciler{Store: a.Store, Provider: a.Provider, Now: a.Now}).Run(ctx)
	return err
}

// Finish : clôt la demande (idempotent).
func (a *Activities) Finish(ctx context.Context, requestID string, ok bool, msg string) error {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid request id", "InvalidRequest", err)
	}
	if err := a.Store.FinishRequest(ctx, id, ok, msg, a.now()); err != nil {
		return err
	}
	r, err := a.Store.Request(ctx, id)
	if err != nil || r == nil || r.VPSID == nil {
		return err
	}
	if v, err := a.Store.VPS(ctx, *r.VPSID); err == nil {
		if err := service.StopIfSuspended(ctx, a.Store, v, r.Kind); err != nil {
			return err
		}
	}
	service.Notify(ctx, a.Store, a.Events, *r.VPSID) // opération finie : le VPS est libre
	return nil
}
