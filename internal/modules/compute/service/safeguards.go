package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// ── Stock ───────────────────────────────────────────────────────────────

// StockView : disponibilité d'un emplacement pour une offre (console).
type StockView struct {
	Status string `json:"status"`
	Days   int    `json:"days,omitempty"`
}

func (s *Service) stockIndex(ctx context.Context) map[string]map[string]domain.Stock {
	out := map[string]map[string]domain.Stock{}
	rows, err := s.Store.Stock(ctx)
	if err != nil {
		return out // sans relevé : aucune restriction (la commande est remboursée en cas de rupture)
	}
	for _, r := range rows {
		if out[r.Offer] == nil {
			out[r.Offer] = map[string]domain.Stock{}
		}
		out[r.Offer][r.Location] = r
	}
	return out
}

// ── Quotas et comptes récents ──────────────────────────────────────────

// checkOrderAllowed : stock, quota de l'organisation, limites des comptes
// récents — vérifiés avant tout débit.
func (s *Service) checkOrderAllowed(ctx context.Context, org uuid.UUID, offer, location string) error {
	if st, ok := s.stockIndex(ctx)[offer][location]; ok && !st.Orderable() {
		return domain.ErrOutOfStock
	}
	count, err := s.Store.CountVPS(ctx, org)
	if err != nil {
		return err
	}
	return s.checkLimits(ctx, org, offer, count)
}

// checkLimits : quota et comptes récents pour count VPS déjà comptés (le
// comptage se refait dans la transaction de commande, sous verrou de
// l'organisation : des commandes simultanées ne dépassent jamais la limite).
func (s *Service) checkLimits(ctx context.Context, org uuid.UUID, offer string, count int) error {
	limits, err := s.Store.Limits(ctx)
	if err != nil {
		return err
	}
	own, err := s.Store.OrgLimits(ctx, org)
	if err != nil {
		return err
	}
	max := limits.MaxVPSPerOrg
	if own != nil {
		max = own.MaxVPS
	}
	if count >= max {
		return domain.ErrQuotaReached
	}
	if own != nil && own.Trusted {
		return nil
	}
	recent, err := s.isRecent(ctx, org, limits)
	if err != nil || !recent {
		return err
	}
	if count >= limits.NewAccountMaxVPS {
		return domain.ErrNewAccountLimit
	}
	allowed := limits.NewAccountOfferList()
	if len(allowed) == 0 {
		return nil
	}
	for _, code := range allowed {
		if domain.NormalizeCode(code) == offer {
			return nil
		}
	}
	return domain.ErrNewAccountLimit
}

// isRecent : organisation créée il y a moins de NewAccountDays, ou sans
// aucune recharge payée (le solde ne vient que de crédits ou de gestes).
func (s *Service) isRecent(ctx context.Context, org uuid.UUID, l *domain.Limits) (bool, error) {
	created, err := s.Store.OrganizationCreatedAt(ctx, org)
	if err != nil {
		return false, err
	}
	if s.now().Sub(created) < time.Duration(l.NewAccountDays)*24*time.Hour {
		return true, nil
	}
	paid, err := s.Store.HasPaidTopup(ctx, org)
	return !paid, err
}

// LimitsUpdate : réglages de la plateforme (équipe).
type LimitsUpdate struct {
	MaxVPSPerOrg     int      `json:"max_vps_per_org" binding:"min=0,max=1000" description:"VPS au plus par organisation"`
	NewAccountDays   int      `json:"new_account_days" binding:"min=0,max=365" description:"Ancienneté en jours en deçà de laquelle un compte est récent"`
	NewAccountMaxVPS int      `json:"new_account_max_vps" binding:"min=0,max=100" description:"VPS au plus pour un compte récent ou sans recharge payée"`
	NewAccountOffers []string `json:"new_account_offers" description:"Offres permises aux comptes récents (vide : toutes)"`
}

func (s *Service) Limits(ctx context.Context) (*domain.Limits, error) { return s.Store.Limits(ctx) }

func (s *Service) SaveLimits(ctx context.Context, in LimitsUpdate, by string) (*domain.Limits, error) {
	codes := []string{}
	for _, c := range in.NewAccountOffers {
		if c = domain.NormalizeCode(c); c != "" {
			codes = append(codes, c)
		}
	}
	raw, _ := json.Marshal(codes)
	l := &domain.Limits{MaxVPSPerOrg: in.MaxVPSPerOrg, NewAccountDays: in.NewAccountDays, NewAccountMaxVPS: in.NewAccountMaxVPS,
		NewAccountOffers: raw, UpdatedBy: by, UpdatedAt: s.now()}
	if err := s.Store.SaveLimits(ctx, l); err != nil {
		return nil, err
	}
	return s.Store.Limits(ctx)
}

// OrgLimitsUpdate : plafond accordé à une organisation (nil : réglages de la plateforme).
type OrgLimitsUpdate struct {
	OrganizationID string `path:"id" json:"-"`
	MaxVPS         *int   `json:"max_vps" description:"VPS au plus pour cette organisation (absent : réglage de la plateforme)"`
	Trusted        bool   `json:"trusted" description:"Exemptée des limites des comptes récents"`
}

// OrgLimitsView : réglages appliqués à une organisation.
type OrgLimitsView struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	MaxVPS         int       `json:"max_vps"`
	Custom         bool      `json:"custom"`
	Trusted        bool      `json:"trusted"`
	Recent         bool      `json:"recent"`
	Count          int       `json:"count"`
}

func (s *Service) OrgLimitsView(ctx context.Context, org uuid.UUID) (*OrgLimitsView, error) {
	limits, err := s.Store.Limits(ctx)
	if err != nil {
		return nil, err
	}
	own, err := s.Store.OrgLimits(ctx, org)
	if err != nil {
		return nil, err
	}
	count, err := s.Store.CountVPS(ctx, org)
	if err != nil {
		return nil, err
	}
	v := &OrgLimitsView{OrganizationID: org, MaxVPS: limits.MaxVPSPerOrg, Count: count}
	if own != nil {
		v.MaxVPS, v.Custom, v.Trusted = own.MaxVPS, true, own.Trusted
	}
	if recent, err := s.isRecent(ctx, org, limits); err == nil {
		v.Recent = recent && !v.Trusted
	}
	return v, nil
}

func (s *Service) SetOrgLimits(ctx context.Context, org uuid.UUID, in OrgLimitsUpdate, by string) (*OrgLimitsView, error) {
	if in.MaxVPS == nil && !in.Trusted {
		if err := s.Store.DeleteOrgLimits(ctx, org); err != nil {
			return nil, err
		}
		return s.OrgLimitsView(ctx, org)
	}
	limits, err := s.Store.Limits(ctx)
	if err != nil {
		return nil, err
	}
	max := limits.MaxVPSPerOrg
	if in.MaxVPS != nil {
		if *in.MaxVPS < 0 || *in.MaxVPS > 1000 {
			return nil, domain.ErrInvalidOffer
		}
		max = *in.MaxVPS
	}
	if err := s.Store.SaveOrgLimits(ctx, &domain.OrgLimits{OrganizationID: org, MaxVPS: max, Trusted: in.Trusted, UpdatedBy: by, UpdatedAt: s.now()}); err != nil {
		return nil, err
	}
	return s.OrgLimitsView(ctx, org)
}

// ── Procédure abus (équipe) ────────────────────────────────────────────

// AbuseRequest : motif montré au client.
type AbuseRequest struct {
	VPSID string `path:"vpsID" json:"-"`
	Note  string `json:"note" binding:"required" description:"Motif de la suspension, montré au client (sans donnée personnelle de tiers)"`
}

// SuspendForAbuse : arrêt immédiat demandé par l'équipe (danger avéré) ;
// le VPS n'est plus facturé tant qu'il est suspendu pour abus.
func (s *Service) SuspendForAbuse(ctx context.Context, id uuid.UUID, note, by string) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.State == domain.StateDeleted {
		return nil, domain.ErrVPSNotFound
	}
	note = strings.TrimSpace(note)
	if r := []rune(note); len(r) > 500 {
		note = string(r[:500])
	}
	resume := v.State
	if v.State == domain.StateSuspended {
		resume = v.ResumeState
	}
	now := s.now()
	suspend := func(stop bool) error {
		return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
			if stop {
				if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindStop, CreatedBy: by}); err != nil {
					return err
				}
			}
			return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"state": domain.StateSuspended, "failure_code": domain.FailureAbuse,
				"abuse_note": note, "resume_state": resume, "updated_at": now})
		})
	}
	err = suspend(v.State == domain.StateRunning && v.ProviderID != "")
	if isBusy(err) {
		err = suspend(false) // l'arrêt part à la fin de l'opération en cours (StopIfSuspended)
	}
	if err != nil {
		return nil, err
	}
	Notify(ctx, s.Store, s.Events, v.ID)
	return s.Store.VPS(ctx, v.ID)
}

// ResumeFromAbuse : fin de la suspension par l'équipe ; mois fini entre-temps :
// le VPS reste suspendu pour impayé jusqu'au paiement.
func (s *Service) ResumeFromAbuse(ctx context.Context, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.State != domain.StateSuspended || v.FailureCode != domain.FailureAbuse {
		return nil, domain.ErrNotAbuseSuspended
	}
	now := s.now()
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		base := map[string]any{"abuse_note": "", "resume_state": "", "updated_at": now}
		switch {
		case v.PaidUntil == nil || !v.PaidUntil.After(now):
			base["failure_code"] = domain.FailureUnpaid
		case v.ResumeState == domain.StateRunning && v.ProviderID != "":
			// vm.start le remet en marche (MarkObserved accepte la reprise d'un impayé).
			if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindStart, CreatedBy: by}); err != nil {
				return err
			}
			base["failure_code"] = domain.FailureUnpaid
		default:
			base["state"], base["failure_code"] = domain.StateStopped, ""
		}
		return s.Store.UpdateVPSTx(tx, v.ID, base)
	})
	if err != nil {
		return nil, err
	}
	Notify(ctx, s.Store, s.Events, v.ID)
	return s.Store.VPS(ctx, v.ID)
}

// AdminTerminate : résiliation par l'équipe (récidive grave).
func (s *Service) AdminTerminate(ctx context.Context, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.terminate(ctx, v, by)
}

// ── E-mails du fournisseur (équipe) ─────────────────────────────────────

func (s *Service) ProviderMails(ctx context.Context, openOnly bool) ([]domain.ProviderMail, error) {
	return s.Store.ProviderMails(ctx, openOnly, 200)
}

func (s *Service) HandleProviderMail(ctx context.Context, id uuid.UUID, by string) error {
	return s.Store.HandleProviderMail(ctx, id, by, s.now())
}

// AttachDelivered : l'équipe rattache un VPS livré (commande en attente de
// son identifiant) à un orphelin du dernier relevé ; le workflow de création
// le reprend à son prochain passage.
func (s *Service) AttachDelivered(ctx context.Context, id uuid.UUID, providerID string) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ProviderID != "" || (v.State != domain.StateCreating && v.State != domain.StatePending) {
		return nil, domain.ErrNotReady
	}
	machines, err := s.Store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	providerID = strings.TrimSpace(providerID)
	for _, m := range machines {
		if m.ProviderID == providerID && m.Drift == domain.DriftOrphan {
			if err := s.Store.UpdateVPS(ctx, v.ID, map[string]any{"provider_id": providerID, "updated_at": s.now()}); err != nil {
				return nil, err
			}
			Notify(ctx, s.Store, s.Events, v.ID)
			return s.Store.VPS(ctx, v.ID)
		}
	}
	return nil, domain.ErrNotOrphan
}
