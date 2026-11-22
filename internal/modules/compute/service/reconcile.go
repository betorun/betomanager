package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// ErrNoProvider : le mode compute n'a pas de clés du fournisseur.
var ErrNoProvider = errors.New("compute: no provider configured")

// Reconciler : rapproche la base du fournisseur. Elle ne corrige que ce qui
// est sûr (marche ↔ arrêt, adresses d'un VPS sans opération en cours) ; le
// reste est relevé pour l'équipe (écarts), jamais réparé en silence.
type Reconciler struct {
	Store    Store
	Provider ComputeProvider
	Now      func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// settled : états que la réconciliation peut corriger d'elle-même.
func settled(state string) bool {
	return state == domain.StateRunning || state == domain.StateStopped
}

// expectedAtProvider : un VPS dans cet état doit exister chez le fournisseur.
func expectedAtProvider(state string) bool {
	switch state {
	case domain.StateRunning, domain.StateStopped, domain.StateSuspended, domain.StateCreating:
		return true
	}
	return false
}

// Run : une passe complète ; elle est enregistrée, réussie ou non.
func (r *Reconciler) Run(ctx context.Context) (*domain.ReconcileRun, error) {
	if r.Provider == nil {
		return nil, ErrNoProvider
	}
	run := &domain.ReconcileRun{ID: uuid.New(), Provider: r.Provider.Name(), StartedAt: r.now()}
	if err := r.Store.SaveRun(ctx, run); err != nil {
		return nil, err
	}
	err := r.pass(ctx, run)
	done := r.now()
	run.FinishedAt = &done
	if err != nil {
		run.Error = err.Error()
		if len(run.Error) > 500 {
			run.Error = run.Error[:500]
		}
	}
	if saveErr := r.Store.SaveRun(ctx, run); saveErr != nil && err == nil {
		err = saveErr
	}
	return run, err
}

func (r *Reconciler) pass(ctx context.Context, run *domain.ReconcileRun) error {
	provider := r.Provider.Name()
	vms, err := r.Provider.List(ctx)
	if err != nil {
		return err
	}
	locals, err := r.Store.ProviderVPS(ctx, provider)
	if err != nil {
		return err
	}
	byID := make(map[string]domain.VPS, len(locals))
	for _, v := range locals {
		byID[v.ProviderID] = v
	}

	seenAt := r.now()
	machines := make([]domain.ProviderMachine, 0, len(vms))
	present := make(map[string]bool, len(vms))
	for _, vm := range vms {
		present[vm.ProviderID] = true
		m := domain.ProviderMachine{
			Provider: provider, ProviderID: vm.ProviderID, State: vm.State, RawState: vm.RawState,
			Plan: vm.Plan, Zone: vm.Zone, IPv4: vm.IPv4, IPv6: vm.IPv6, Busy: vm.Busy, SeenAt: seenAt,
		}
		local, known := byID[vm.ProviderID]
		switch {
		case !known:
			m.Drift = domain.DriftOrphan
			run.Orphans++
		case vm.Busy:
			// Opération en cours chez le fournisseur : on relèvera à la passe suivante.
			id := local.ID
			m.VPSID = &id
		case settled(local.State) && settled(vm.State):
			id := local.ID
			m.VPSID = &id
			ipv4, ipv6 := keep(vm.IPv4, local.IPv4), keep(vm.IPv6, local.IPv6)
			if local.State != vm.State || local.IPv4 != ipv4 || local.IPv6 != ipv6 {
				ok, err := r.Store.ApplyObserved(ctx, local.ID, local.Version, vm.State, ipv4, ipv6, seenAt)
				if err != nil {
					return err
				}
				if ok {
					run.Updated++
				}
			}
		default:
			id := local.ID
			m.VPSID = &id
			// En création : le workflow de commande s'en charge.
			if local.State != domain.StateCreating && local.State != vm.State {
				m.Drift = domain.DriftState
				run.Drifts++
			}
		}
		machines = append(machines, m)
	}
	run.Seen = len(machines)
	for _, v := range locals {
		if !present[v.ProviderID] && expectedAtProvider(v.State) {
			run.Missing++
		}
	}
	return r.Store.ReplaceMachines(ctx, provider, machines)
}

// keep : une adresse absente du relevé ne remplace pas celle connue.
func keep(observed, known string) string {
	if observed == "" {
		return known
	}
	return observed
}
