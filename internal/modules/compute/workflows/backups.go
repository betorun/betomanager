package workflows

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// Délais de l'option snapshot.
const (
	optionTimeout = 2 * time.Hour
	optionPoll    = time.Minute
	// StuckSnapshotAfter : au-delà, la réconciliation annule un snapshot ou
	// une sauvegarde en cours (abortSnapshot).
	StuckSnapshotAfter = 6 * time.Hour
)

// backupVM : restauration, montage, démontage, snapshot (créer, revenir,
// supprimer) ; le relevé des sauvegardes est refait à la fin.
func backupVM(ctx workflow.Context, requestID, kind string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if spec.ProviderID == "" {
		return nil
	}
	if err := waitReady(ctx, spec.ProviderID); err != nil {
		return err
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.BackupAction, spec, kind).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.RefreshBackups, spec.VPSID).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.Observe, spec.VPSID).Get(ctx, nil)
}

// subscribeSnapshot : commande de l'option (une tentative), livraison, option
// visible sur le VPS. Échec : mois remboursé, option retirée.
func subscribeSnapshot(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	err := runSubscribe(ctx, &spec)
	if err == nil {
		return nil
	}
	if ferr := workflow.ExecuteActivity(quick(ctx), a.FailSubscribe, spec).Get(ctx, nil); ferr != nil {
		return ferr
	}
	return err
}

func runSubscribe(ctx workflow.Context, spec *VMSpec) error {
	var a *Activities
	var orderID string
	if err := workflow.ExecuteActivity(once(ctx), a.OrderSnapshot, spec.VPSID).Get(ctx, &orderID); err != nil {
		return err
	}
	deadline := workflow.Now(ctx).Add(optionTimeout)
	for {
		var st OrderStatus
		if err := workflow.ExecuteActivity(quick(ctx), a.OrderState, orderID).Get(ctx, &st); err != nil {
			return err
		}
		if st.State == service.OrderFailed {
			return temporal.NewNonRetryableApplicationError("provider option order failed", errTypeProvider, nil)
		}
		var active bool
		if err := workflow.ExecuteActivity(quick(ctx), a.SnapshotOptionActive, spec.ProviderID).Get(ctx, &active); err != nil {
			return err
		}
		if active {
			break
		}
		if workflow.Now(ctx).After(deadline) {
			return temporal.NewNonRetryableApplicationError("snapshot option not delivered in time", errTypeTimeout, nil)
		}
		if err := workflow.Sleep(ctx, optionPoll); err != nil {
			return err
		}
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.SetSnapshotOption, spec.VPSID, domain.SnapshotActive).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.RefreshBackups, spec.VPSID).Get(ctx, nil)
}

// releaseSnapshot : option rendue ; elle n'est plus renouvelée.
func releaseSnapshot(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.ReleaseSnapshot, spec.VPSID).Get(ctx, nil); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(quick(ctx), a.SetSnapshotOption, spec.VPSID, "").Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.RefreshBackups, spec.VPSID).Get(ctx, nil)
}

// ── Activités ───────────────────────────────────────────────────────────

func (a *Activities) backups() (service.BackupProvider, error) {
	if a.Backups == nil {
		return nil, nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	return a.Backups, nil
}

// BackupAction : l'opération demandée, chez le fournisseur.
func (a *Activities) BackupAction(ctx context.Context, spec VMSpec, kind string) (string, error) {
	b, err := a.backups()
	if err != nil {
		return "", err
	}
	var task string
	switch kind {
	case domain.KindRestore:
		task, err = b.Restore(ctx, spec.ProviderID, spec.RestorePoint, spec.Mode != domain.RestoreMount)
	case domain.KindUnmount:
		task, err = b.Unmount(ctx, spec.ProviderID, spec.RestorePoint)
	case domain.KindSnapshotCreate:
		task, err = b.CreateSnapshot(ctx, spec.ProviderID, spec.Description)
	case domain.KindSnapshotRevert:
		task, err = b.RevertSnapshot(ctx, spec.ProviderID)
	case domain.KindSnapshotDelete:
		task, err = b.DeleteSnapshot(ctx, spec.ProviderID)
	default:
		return "", nonRetryable("unknown backup operation", "UnknownKind", nil)
	}
	if err != nil {
		return "", nonRetryable(err.Error(), errTypeProvider, err)
	}
	return task, nil
}

// RefreshBackups : relevé des sauvegardes du VPS, écrit pour l'API.
func (a *Activities) RefreshBackups(ctx context.Context, vpsID string) error {
	b, err := a.backups()
	if err != nil {
		return err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	if v.ProviderID == "" {
		return nil
	}
	pb, err := b.Backups(ctx, v.ProviderID)
	if err != nil {
		return err
	}
	if _, err := service.SaveBackups(ctx, a.Store, v.ID, pb, a.now()); err != nil {
		return err
	}
	if a.Events != nil {
		a.Events.BackupsChanged(ctx, v)
	}
	return nil
}

// Observe : état relevé chez le fournisseur (une restauration redémarre le VPS).
func (a *Activities) Observe(ctx context.Context, vpsID string) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	vm, err := p.Get(ctx, v.ProviderID)
	if err != nil || vm == nil {
		return err
	}
	state := vm.State
	if state != domain.StateRunning && state != domain.StateStopped {
		state = v.State
	}
	return a.MarkObserved(ctx, vpsID, state)
}

// OrderSnapshot : option commandée (jamais rejouée).
func (a *Activities) OrderSnapshot(ctx context.Context, vpsID string) (string, error) {
	b, err := a.backups()
	if err != nil {
		return "", err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return "", err
	}
	o, err := a.Store.Offer(ctx, v.Offer)
	if err != nil {
		return "", err
	}
	if o.SnapshotPlanCode == "" {
		return "", nonRetryable("snapshot option not sold", errTypeProvider, nil)
	}
	id, err := b.OrderSnapshotOption(ctx, v.ProviderID, o.SnapshotPlanCode, o.SnapshotPriceCents)
	if err != nil {
		if errors.Is(err, service.ErrProviderPriceAbove) {
			return "", nonRetryable(err.Error(), errTypePriceAbove, err)
		}
		return "", nonRetryable(err.Error(), errTypeProvider, err)
	}
	return id, nil
}

func (a *Activities) SnapshotOptionActive(ctx context.Context, providerID string) (bool, error) {
	b, err := a.backups()
	if err != nil {
		return false, err
	}
	pb, err := b.Backups(ctx, providerID)
	if err != nil {
		return false, err
	}
	return pb.SnapshotOption, nil
}

func (a *Activities) ReleaseSnapshot(ctx context.Context, vpsID string) error {
	b, err := a.backups()
	if err != nil {
		return err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	plan := ""
	if o, err := a.Store.Offer(ctx, v.Offer); err == nil {
		plan = o.SnapshotPlanCode
	}
	return b.ReleaseSnapshotOption(ctx, v.ProviderID, plan)
}

func (a *Activities) SetSnapshotOption(ctx context.Context, vpsID, state string) error {
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"snapshot_option": state, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

// FailSubscribe : option non obtenue : mois remboursé, option retirée.
func (a *Activities) FailSubscribe(ctx context.Context, spec VMSpec) error {
	v, err := a.vps(ctx, spec.VPSID)
	if err != nil {
		return err
	}
	if a.Wallet != nil && spec.AmountMilli > 0 && spec.Period != "" {
		if err := a.Wallet.RefundCompute(ctx, v.OrganizationID, billingDomain.Milli(spec.AmountMilli), spec.Period+":refund",
			"Snapshot du VPS "+v.Name+" non obtenu, remboursé"); err != nil {
			return err
		}
	}
	return a.SetSnapshotOption(ctx, spec.VPSID, "")
}
