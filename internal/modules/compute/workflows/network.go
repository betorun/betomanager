package workflows

import (
	"context"
	"encoding/json"

	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// reverseVM : DNS inverse d'une adresse.
func reverseVM(ctx workflow.Context, requestID string) error {
	var a *Activities
	return workflow.ExecuteActivity(quick(ctx), a.ApplyReverse, requestID).Get(ctx, nil)
}

// unblockSpam : blocage anti-spam levé par l'équipe.
func unblockSpam(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.UnblockSpam, spec.VPSID).Get(ctx, nil)
}

// terminateVM : arrêt tout de suite, renouvellement coupé chez le
// fournisseur (suppression à l'expiration du mois déjà payé).
func terminateVM(ctx workflow.Context, requestID string) error {
	var a *Activities
	var spec VMSpec
	if err := workflow.ExecuteActivity(quick(ctx), a.LoadVM, requestID).Get(ctx, &spec); err != nil {
		return err
	}
	if spec.ProviderID == "" {
		return nil
	}
	var task string
	if err := workflow.ExecuteActivity(quick(ctx), a.Power, spec.ProviderID, false).Get(ctx, &task); err != nil {
		return err
	}
	if err := waitTask(ctx, spec.ProviderID, task); err != nil {
		return err
	}
	return workflow.ExecuteActivity(quick(ctx), a.StopRenewal, spec.ProviderID).Get(ctx, nil)
}

func (a *Activities) network() (service.NetworkProvider, error) {
	if a.Network == nil {
		return nil, nonRetryable("no compute provider configured", "NoProvider", nil)
	}
	return a.Network, nil
}

// ApplyReverse : DNS inverse demandé (le fournisseur vérifie le nom).
func (a *Activities) ApplyReverse(ctx context.Context, requestID string) error {
	n, err := a.network()
	if err != nil {
		return err
	}
	id, err := parseUUID(requestID)
	if err != nil {
		return err
	}
	r, err := a.Store.Request(ctx, id)
	if err != nil || r == nil || r.VPSID == nil {
		return nonRetryable("compute request without VPS", "RequestNotFound", err)
	}
	var p domain.ReversePayload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nonRetryable("invalid reverse payload", "InvalidRequest", err)
	}
	v, err := a.Store.VPS(ctx, *r.VPSID)
	if err != nil {
		return err
	}
	ip, field := v.IPv4, "reverse_v4"
	if p.Family == "v6" {
		ip, field = v.IPv6, "reverse_v6"
	}
	if err := n.SetReverse(ctx, ip, p.Reverse); err != nil {
		return nonRetryable(err.Error(), errTypeProvider, err)
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{field: p.Reverse, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

func (a *Activities) UnblockSpam(ctx context.Context, vpsID string) error {
	n, err := a.network()
	if err != nil {
		return err
	}
	v, err := a.vps(ctx, vpsID)
	if err != nil {
		return err
	}
	if err := n.UnblockSpam(ctx, v.IPv4); err != nil {
		return nonRetryable(err.Error(), errTypeProvider, err)
	}
	if err := a.Store.UpdateVPS(ctx, v.ID, map[string]any{"spam_blocked_at": nil, "updated_at": a.now()}); err != nil {
		return err
	}
	service.Notify(ctx, a.Store, a.Events, v.ID)
	return nil
}

func (a *Activities) StopRenewal(ctx context.Context, providerID string) error {
	n, err := a.network()
	if err != nil {
		return err
	}
	return n.StopRenewal(ctx, providerID)
}
