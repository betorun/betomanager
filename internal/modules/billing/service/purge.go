package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Suppression des données d'une organisation suspendue trop longtemps
// (ADR 0012) : trois avertissements, puis conteneurs et disques, bases (après
// une copie finale) et sites supprimés. L'organisation, ses projets, son
// grand livre et ses factures restent : une recharge la réactive, vide.

// DataPurger : supprime les ressources d'une organisation. Rend une erreur
// tant que tout n'est pas supprimé (ou en cours de suppression) : la purge
// est reprise au passage suivant. Les copies finales des bases sont gardées
// jusqu'à archiveUntil.
type DataPurger interface {
	PurgeOrganization(ctx context.Context, org uuid.UUID, archiveUntil time.Time) error
}

// PurgeAlerter : e-mails aux propriétaires (avertissement, suppression faite).
type PurgeAlerter interface {
	PurgeNotice(ctx context.Context, org uuid.UUID, stage int, purgeAt time.Time)
}

// DefaultPurgeEvery : fréquence de la purge (processus API : la copie finale
// des bases arrive par une URL signée de l'API, comme celle de la remise à zéro).
const DefaultPurgeEvery = time.Hour

// RunPurge : avertissements et suppressions, à intervalle régulier.
func (s *Service) RunPurge(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultPurgeEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.PurgeSuspended(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// PurgeSuspended : un passage sur les organisations suspendues.
func (s *Service) PurgeSuspended(ctx context.Context) {
	if s.Purges == nil || s.Purger == nil {
		return
	}
	rows, err := s.Purges.Suspended(ctx)
	if err != nil {
		s.log().Warnw("billing: suspended organizations not listed", "error", err)
		return
	}
	now := s.now()
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		c, err := s.catalog(ctx, r.OrganizationID)
		if err != nil {
			s.log().Warnw("billing: purge skipped (catalog)", "organization", r.OrganizationID, "error", err)
			continue
		}
		stage := domain.PurgeStage(r.SuspendedAt, c.PurgeDays, now)
		purgeAt := domain.PurgeAt(r.SuspendedAt, c.PurgeDays)
		if stage == domain.PurgeStageNone {
			continue
		}
		if stage < domain.PurgeStageDue {
			if stage > r.PurgeStage {
				// Un seul e-mail, celui de l'étape atteinte (pas de rafale si
				// le processus était arrêté).
				s.notifyStage(ctx, r.OrganizationID, stage, purgeAt, now)
			}
			continue
		}
		s.purge(ctx, r, c.PurgeDays, now)
	}
}

// notifyStage : étape retenue, puis l'e-mail (jamais deux fois le même).
func (s *Service) notifyStage(ctx context.Context, org uuid.UUID, stage int, purgeAt, now time.Time) {
	if err := s.Purges.SetStage(ctx, org, stage, now); err != nil {
		s.log().Warnw("billing: purge notice not recorded", "organization", org, "error", err)
		return
	}
	if s.PurgeAlerts != nil {
		s.PurgeAlerts.PurgeNotice(ctx, org, stage, purgeAt)
	}
}

// purge : date passée. Jamais de suppression sans le dernier avertissement
// envoyé depuis au moins PurgeLastNotice (avertissements manqués, délai
// raccourci par l'équipe, processus arrêté).
func (s *Service) purge(ctx context.Context, r repository.PurgeRow, purgeDays int, now time.Time) {
	org := r.OrganizationID
	if r.PurgeStage < domain.PurgeStageLast {
		s.notifyStage(ctx, org, domain.PurgeStageLast, now.Add(domain.PurgeLastNotice), now)
		return
	}
	if r.PurgeStage == domain.PurgeStageLast {
		if r.PurgeNoticedAt == nil || now.Before(r.PurgeNoticedAt.Add(domain.PurgeLastNotice)) {
			return
		}
		if err := s.Purges.SetStage(ctx, org, domain.PurgeStageDue, now); err != nil {
			s.log().Warnw("billing: purge not started", "organization", org, "error", err)
			return
		}
	}
	// Les copies finales des bases restent autant de jours que la suspension.
	if err := s.Purger.PurgeOrganization(ctx, org, now.AddDate(0, 0, purgeDays)); err != nil {
		s.log().Warnw("billing: organization data not fully deleted (retried)", "organization", org, "error", err)
		return
	}
	if err := s.Purges.SetPurged(ctx, org, now); err != nil {
		s.log().Warnw("billing: purge not recorded", "organization", org, "error", err)
		return
	}
	s.log().Warnw("billing: organization data deleted after suspension", "organization", org)
	if s.PurgeAlerts != nil {
		s.PurgeAlerts.PurgeNotice(ctx, org, domain.PurgeStageDue, now)
	}
}
