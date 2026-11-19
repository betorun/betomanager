package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Plans (ADR 0012, phase 5) : un plan fixe les limites des projets de
// l'organisation et un abonnement mensuel, mesuré comme une ressource.

// ProjectLimiter : limites d'un projet (service des projets : base, puis
// quota Kubernetes par un workflow).
type ProjectLimiter interface {
	SetLimits(ctx context.Context, project uuid.UUID, cpu, memory, storage string) error
}

func (s *Service) currency(ctx context.Context, raw string) string {
	if c := domain.NormalizeCurrency(raw); c != "" {
		return c
	}
	return domain.DefaultCurrency
}

// PublicPlans : plans proposés aux clients, dans une monnaie (page Tarifs).
func (s *Service) PublicPlans(ctx context.Context, currency string) (*domain.PlansResponse, error) {
	plans, err := s.Plans.Plans(ctx)
	if err != nil {
		return nil, err
	}
	cur := s.currency(ctx, currency)
	out := &domain.PlansResponse{Plans: []domain.PlanView{}}
	for _, p := range plans {
		if p.Public {
			out.Plans = append(out.Plans, p.View(cur))
		}
	}
	return out, nil
}

// OrgPlan : plan de l'organisation et plans proposés.
func (s *Service) OrgPlan(ctx context.Context, org uuid.UUID) (*domain.OrgPlan, error) {
	id, since, err := s.Plans.OrgPlan(ctx, org)
	if err != nil {
		return nil, err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	plans, err := s.Plans.Plans(ctx)
	if err != nil {
		return nil, err
	}
	out := &domain.OrgPlan{Since: since, Plans: []domain.PlanView{}}
	for _, p := range plans {
		if p.ID == id {
			out.Current = p.View(acc.Currency)
		}
		if p.Public || p.ID == id {
			out.Plans = append(out.Plans, p.View(acc.Currency))
		}
	}
	return out, nil
}

// ChangePlan : change le plan de l'organisation et met les limites de tous
// ses projets au niveau du plan. Refusé si un projet réserve déjà plus que les
// nouvelles limites (rien n'est alors modifié). Un plan payant demande la
// facturation réelle active ; un plan non public n'est attribué que par
// l'équipe (staff).
func (s *Service) ChangePlan(ctx context.Context, org uuid.UUID, planID, by string, staff bool) (*domain.OrgPlan, error) {
	p, err := s.Plans.Plan(ctx, strings.TrimSpace(planID))
	if err != nil {
		return nil, err
	}
	if !p.Public && !staff {
		return nil, domain.ErrPlanNotFound
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	if p.Fee(acc.Currency) > 0 && !acc.Charging && !staff {
		return nil, domain.ErrPlanNeedsBilling
	}
	cpu, mem, err := p.Limits()
	if err != nil {
		return nil, err
	}
	usages, err := s.Inventory.ProjectUsages(ctx, org)
	if err != nil {
		return nil, err
	}
	// Contrôle de tous les projets d'abord (sauf ceux qu'une exception de
	// l'équipe soustrait au plan) : un plan ne s'applique jamais à moitié.
	overridden, err := s.overridden(ctx, org)
	if err != nil {
		return nil, err
	}
	var over []string
	for _, u := range usages {
		if !overridden[u.ProjectID] && (u.CPUMilli > cpu || u.MemMi > mem) {
			over = append(over, u.Name)
		}
	}
	if len(over) > 0 {
		return nil, &domain.PlanBelowUsageError{Projects: over}
	}
	if err := s.Plans.SetOrgPlan(ctx, org, p.ID, s.now()); err != nil {
		return nil, err
	}
	s.log().Infow("billing: plan changed", "organization", org, "plan", p.ID, "by", by, "staff", staff)
	if _, missed, err := s.ReconcileLimits(ctx, &org, nil); err != nil {
		return nil, err
	} else if len(missed) > 0 {
		// Contrôlé juste avant : un conteneur ajouté entre-temps. Le
		// rapprochement réessaiera.
		s.log().Warnw("billing: plan limits not applied to every project yet", "organization", org, "projects", len(missed))
	}
	return s.OrgPlan(ctx, org)
}

// overridden : projets de l'organisation soustraits au plan par l'équipe.
func (s *Service) overridden(ctx context.Context, org uuid.UUID) (map[uuid.UUID]bool, error) {
	ids, err := s.Plans.Overrides(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// ReconcileLimits : ramène chaque projet aux limites de sa cible (exception
// de l'équipe, sinon plan de son organisation). Plan modifié, nouveau
// projet, plan changé : tout passe par ici. Un projet qui réserve déjà plus
// que sa cible garde ses limites (il est rendu dans notApplied) ; le
// rapprochement suivant réessaie. org / plan : restreindre.
func (s *Service) ReconcileLimits(ctx context.Context, org *uuid.UUID, plan *string) (int, []domain.NotApplied, error) {
	if s.Limits == nil || s.Plans == nil {
		return 0, nil, nil
	}
	targets, err := s.Inventory.LimitTargets(ctx, org, plan)
	if err != nil {
		return 0, nil, err
	}
	applied := 0
	missed := []domain.NotApplied{}
	for _, t := range targets {
		err := s.Limits.SetLimits(ctx, t.ProjectID, t.CPU, t.Memory, t.Storage)
		switch {
		case err == nil:
			applied++
		case isBelowUsage(err):
			missed = append(missed, domain.NotApplied{ProjectID: t.ProjectID.String(), Name: t.Name})
		default:
			// Ajustement déjà en cours, cluster indisponible… : au passage suivant.
			s.log().Debugw("billing: project limits not reconciled yet", "project", t.ProjectID, "error", err)
		}
	}
	return applied, missed, nil
}

// LimitsBelowUsage : reconnu sans importer le module des projets (le
// limiteur enveloppe son erreur).
type LimitsBelowUsage interface{ BelowUsage() bool }

func isBelowUsage(err error) bool {
	var b LimitsBelowUsage
	return errors.As(err, &b) && b.BelowUsage()
}

// RecordOverride : l'équipe a posé des limites à la main pour un projet
// (page « Limites des projets ») ; le rapprochement les respectera.
func (s *Service) RecordOverride(ctx context.Context, projectID, cpu, memory, storage, by string) error {
	if s.Plans == nil {
		return nil
	}
	id, err := uuid.Parse(projectID)
	if err != nil {
		return nil
	}
	return s.Plans.SetOverride(ctx, id, cpu, memory, storage, by, s.now())
}

// ClearOverride : le projet revient aux limites du plan de son organisation.
func (s *Service) ClearOverride(ctx context.Context, project uuid.UUID) ([]domain.NotApplied, error) {
	if _, err := s.Plans.DeleteOverride(ctx, project); err != nil {
		return nil, err
	}
	org, err := s.Inventory.ProjectOrg(ctx, project)
	if err != nil {
		return nil, err
	}
	_, missed, err := s.ReconcileLimits(ctx, &org, nil)
	return missed, err
}

// AdminPlans : tous les plans (administration).
func (s *Service) AdminPlans(ctx context.Context) (*domain.AdminPlansResponse, error) {
	plans, err := s.Plans.Plans(ctx)
	if err != nil {
		return nil, err
	}
	out := &domain.AdminPlansResponse{Plans: make([]domain.AdminPlan, 0, len(plans))}
	for _, p := range plans {
		out.Plans = append(out.Plans, p.Admin())
	}
	return out, nil
}

// SavePlan : crée ou modifie un plan (équipe). Ses nouvelles limites
// s'appliquent aussitôt aux projets des organisations qui l'ont (sauf
// exception de l'équipe) ; ceux qui réservent déjà plus sont rendus dans
// NotApplied et le rapprochement réessaie à chaque passage.
func (s *Service) SavePlan(ctx context.Context, u domain.PlanUpdate, by string) (*domain.SavePlanResponse, error) {
	p, err := domain.PlanFromUpdate(u)
	if err != nil {
		return nil, err
	}
	if err := s.Plans.SavePlan(ctx, p, by); err != nil {
		return nil, err
	}
	applied, missed, err := s.ReconcileLimits(ctx, nil, &p.ID)
	if err != nil {
		return nil, err
	}
	s.log().Infow("billing: plan saved", "plan", p.ID, "by", by, "projects_updated", applied, "projects_pending", len(missed))
	return &domain.SavePlanResponse{Plan: p.Admin(), Applied: applied, NotApplied: missed}, nil
}
