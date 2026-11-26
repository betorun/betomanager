package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// Events : un VPS a changé (temps réel de la console).
type Events interface {
	VPSChanged(ctx context.Context, v *domain.VPS)
	// BackupsChanged : nouveau relevé des sauvegardes (la console le relit).
	BackupsChanged(ctx context.Context, v *domain.VPS)
}

// Notify : relit le VPS et le publie ; sans effet si events est nil.
func Notify(ctx context.Context, store Store, events Events, id uuid.UUID) {
	if events == nil {
		return
	}
	if v, err := store.VPS(ctx, id); err == nil {
		events.VPSChanged(ctx, v)
	}
}

// Actions du client sur un VPS.
const (
	ActionStart     = "start"
	ActionStop      = "stop"
	ActionReboot    = "reboot"
	ActionReinstall = "reinstall"
)

var actionKinds = map[string]struct{ kind, op string }{
	ActionStart:     {domain.KindStart, domain.OpStarting},
	ActionStop:      {domain.KindStop, domain.OpStopping},
	ActionReboot:    {domain.KindReboot, domain.OpRebooting},
	ActionReinstall: {domain.KindReinstall, domain.OpReinstalling},
}

// allowed : l'action a un sens dans cet état.
func allowed(action, state string) bool {
	switch action {
	case ActionStart:
		return state == domain.StateStopped
	case ActionStop, ActionReboot:
		return state == domain.StateRunning
	case ActionReinstall:
		return state == domain.StateRunning || state == domain.StateStopped
	}
	return false
}

// ReinstallRequest : réinstallation (efface le disque).
type ReinstallRequest struct {
	Image        string `json:"image" binding:"required" description:"Système (ubuntu-24.04, debian-12)"`
	SSHPublicKey string `json:"ssh_public_key" binding:"required" description:"Clé publique SSH installée (aucun mot de passe n'est créé)"`
}

// Act : dépose l'action (outbox) et marque l'opération, ensemble. Une seule
// opération à la fois par VPS : la base refuse la seconde (ErrBusy).
func (s *Service) Act(ctx context.Context, org, id uuid.UUID, action string, reinstall *ReinstallRequest, by string) (*domain.VPS, error) {
	spec, ok := actionKinds[action]
	if !ok {
		return nil, domain.ErrNotReady
	}
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if err := ready(v); err != nil {
		return nil, err
	}
	if !allowed(action, v.State) {
		return nil, domain.ErrNotReady
	}
	payload := []byte(`{}`)
	if action == ActionReinstall {
		if reinstall == nil {
			return nil, domain.ErrNotSold
		}
		image := strings.ToLower(strings.TrimSpace(reinstall.Image))
		o, err := s.Store.Offer(ctx, v.Offer)
		if err != nil {
			return nil, err
		}
		if o.ImageMap()[image] == "" {
			return nil, domain.ErrNotSold
		}
		key, err := domain.ParseSSHKey(reinstall.SSHPublicKey)
		if err != nil {
			return nil, err
		}
		if payload, err = json.Marshal(domain.ReinstallPayload{Image: image, SSHPublicKey: key.Line, Fingerprint: key.Fingerprint}); err != nil {
			return nil, err
		}
	}
	out, err := s.enqueueOp(ctx, v, spec.kind, spec.op, payload, by, nil, nil)
	if err == nil && action == ActionReinstall && s.VPC != nil {
		// Réseau privé : la clé du système effacé ne vaut plus, tout de suite.
		// Un échec ici est rattrapé à l'installation (clé retirée en base)
		// puis par la réconciliation du réseau.
		_ = s.VPC.BeforeReinstall(ctx, v.ID)
	}
	return out, err
}

// ready : contrôles communs à toute opération du client sur un VPS livré.
func ready(v *domain.VPS) error {
	switch {
	case v.State == domain.StateSuspended:
		return domain.ErrSuspended
	case v.Operation != "":
		return domain.ErrBusy
	case v.Rescue != "":
		return domain.ErrInRescue
	case v.ProviderID == "" || (v.State != domain.StateRunning && v.State != domain.StateStopped):
		return domain.ErrNotReady
	}
	return nil
}

// enqueueOp : demande (outbox) + opération marquée (+ champs) dans une
// transaction ; before s'exécute dans la même transaction (débit…).
func (s *Service) enqueueOp(ctx context.Context, v *domain.VPS, kind, op string, payload []byte, by string,
	fields map[string]any, before func(tx *gorm.DB) error) (*domain.VPS, error) {
	f := map[string]any{"operation": op, "updated_at": s.now()}
	for k, val := range fields {
		f[k] = val
	}
	if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if before != nil {
			if err := before(tx); err != nil {
				return err
			}
		}
		if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: kind, Payload: payload, CreatedBy: by}); err != nil {
			return err
		}
		return s.Store.UpdateVPSTx(tx, v.ID, f)
	}); err != nil {
		return nil, err
	}
	Notify(ctx, s.Store, s.Events, v.ID)
	return s.Store.VPS(ctx, v.ID)
}

// ── Suspension de l'organisation (module billing) ───────────────────────

// SuspendOrganization : solde épuisé, grâce finie : les VPS en marche ou
// arrêtés de l'organisation sont suspendus (et arrêtés) ; leur état est
// retenu pour la reprise. Un VPS occupé par une opération est suspendu
// quand même : la fin de cette opération dépose son arrêt (StopIfSuspended).
func (s *Service) SuspendOrganization(ctx context.Context, org uuid.UUID) error {
	list, err := s.Store.OrganizationVPS(ctx, org)
	if err != nil {
		return err
	}
	now := s.now()
	var first error
	for _, v := range list {
		if v.State != domain.StateRunning && v.State != domain.StateStopped {
			continue
		}
		v := v
		suspend := func(stop bool) error {
			return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
				if stop {
					if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: org, VPSID: &v.ID, Kind: domain.KindStop, CreatedBy: "billing"}); err != nil {
						return err
					}
				}
				return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"state": domain.StateSuspended, "failure_code": domain.FailureOrgSuspended,
					"resume_state": v.State, "updated_at": now})
			})
		}
		err := suspend(v.State == domain.StateRunning)
		if isBusy(err) {
			err = suspend(false)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		Notify(ctx, s.Store, s.Events, v.ID)
	}
	return first
}

// ResumeOrganization : fin de la suspension. Mois encore payé : le VPS
// retrouve son état (relancé s'il tournait). Mois fini pendant la
// suspension : il reste suspendu pour impayé, le renouvellement le relancera.
func (s *Service) ResumeOrganization(ctx context.Context, org uuid.UUID) error {
	list, err := s.Store.OrganizationVPS(ctx, org)
	if err != nil {
		return err
	}
	now := s.now()
	var first error
	for _, v := range list {
		if v.State != domain.StateSuspended || v.FailureCode != domain.FailureOrgSuspended {
			continue
		}
		v := v
		err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
			if v.PaidUntil == nil || !v.PaidUntil.After(now) {
				return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"failure_code": domain.FailureUnpaid, "resume_state": "", "updated_at": now})
			}
			if v.ResumeState == domain.StateRunning {
				// Relancé : il reste suspendu jusqu'au démarrage effectif (vm.start le passe en marche).
				if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: org, VPSID: &v.ID, Kind: domain.KindStart, CreatedBy: "billing"}); err != nil {
					return err
				}
				return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"failure_code": domain.FailureUnpaid, "resume_state": "", "updated_at": now})
			}
			return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"state": domain.StateStopped, "failure_code": "", "resume_state": "", "updated_at": now})
		})
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		Notify(ctx, s.Store, s.Events, v.ID)
	}
	return first
}

// StopIfSuspended : appelé à la fin de chaque opération. Un VPS suspendu
// pendant qu'elle tournait reçoit sa demande d'arrêt maintenant (sans
// effet s'il est déjà arrêté).
func StopIfSuspended(ctx context.Context, store Store, v *domain.VPS, finishedKind string) error {
	if v.State != domain.StateSuspended || finishedKind == domain.KindStop || v.ProviderID == "" {
		return nil
	}
	err := store.Transaction(ctx, func(tx *gorm.DB) error {
		return store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindStop, CreatedBy: "billing"})
	})
	if isBusy(err) {
		return nil
	}
	return err
}

// isBusy : une autre opération occupe le VPS ; à reprendre plus tard.
func isBusy(err error) bool { return errors.Is(err, domain.ErrBusy) }
