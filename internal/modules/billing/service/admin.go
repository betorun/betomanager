package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Administration de la facturation : chaque geste de l'équipe passe par le
// grand livre, avec un motif. Le journal d'audit de la plateforme garde qui
// l'a fait (routes /admin, journalisées comme les autres).

const (
	maxCreditDays = 366
	maxReasonLen  = 300
)

// Orgs : recherche d'organisations, avec leur solde et le coût du mois.
func (s *Service) Orgs(ctx context.Context, query string, limit int) (*domain.OrgsResponse, error) {
	if limit <= 0 || limit > maxPage {
		limit = 50
	}
	rows, err := s.Admin.SearchOrgs(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := &domain.OrgsResponse{Organizations: make([]domain.OrgSummary, 0, len(rows))}
	for _, r := range rows {
		sum, err := s.orgSummary(ctx, r)
		if err != nil {
			return nil, err
		}
		out.Organizations = append(out.Organizations, sum)
	}
	return out, nil
}

func (s *Service) orgSummary(ctx context.Context, r repository.OrgRow) (domain.OrgSummary, error) {
	sum := domain.OrgSummary{ID: r.ID.String(), Name: r.Name, Kind: r.Kind, Owner: r.Owner, Projects: r.Projects,
		Currency: r.Currency, Charging: r.Charging, Balance: domain.Milli(r.Balance).Units(), CreatedAt: r.CreatedAt}
	e, err := s.Estimate(ctx, r.ID, "")
	if err != nil {
		return sum, err
	}
	sum.MonthEstimate = e.Total
	return sum, nil
}

// Org : fiche d'une organisation.
func (s *Service) Org(ctx context.Context, id uuid.UUID) (*domain.OrgDetail, error) {
	r, err := s.Admin.Org(ctx, id)
	if err != nil {
		return nil, err
	}
	sum, err := s.orgSummary(ctx, *r)
	if err != nil {
		return nil, err
	}
	projects, err := s.Admin.OrgProjects(ctx, id)
	if err != nil {
		return nil, err
	}
	w, err := s.Wallet(ctx, id)
	if err != nil {
		return nil, err
	}
	e, err := s.Estimate(ctx, id, "")
	if err != nil {
		return nil, err
	}
	txs, err := s.Transactions(ctx, id, "", 20)
	if err != nil {
		return nil, err
	}
	topups, err := s.Topups(ctx, id)
	if err != nil {
		return nil, err
	}
	return &domain.OrgDetail{OrgSummary: sum, Projects: projects, Wallet: *w, Estimate: *e,
		Transactions: txs.Transactions, Topups: topups.Topups}, nil
}

func (s *Service) requireOrg(ctx context.Context, id uuid.UUID) error {
	_, err := s.Admin.Org(ctx, id)
	return err
}

func reason(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return "", domain.ErrReasonRequired
	}
	if len(r) > maxReasonLen {
		r = r[:maxReasonLen]
	}
	return r, nil
}

// GrantCredit : crédit offert, valable `days` jours, consommé avant l'argent.
func (s *Service) GrantCredit(ctx context.Context, org uuid.UUID, req domain.CreditRequest, by string) (*domain.Wallet, error) {
	why, err := reason(req.Reason)
	if err != nil {
		return nil, err
	}
	if req.Amount <= 0 || req.Days <= 0 || req.Days > maxCreditDays {
		return nil, domain.ErrInvalidAdminAmount
	}
	if err := s.requireOrg(ctx, org); err != nil {
		return nil, err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	now := s.now()
	g := &domain.CreditGrant{ID: uuid.New(), OrganizationID: org, Amount: domain.FromUnits(req.Amount), Currency: acc.Currency,
		ExpiresAt: now.Add(time.Duration(req.Days) * 24 * time.Hour), Reason: why}
	if _, err := s.Ledger.GrantCredit(ctx, g, domain.GrantReference(g.ID), by, now); err != nil {
		return nil, err
	}
	s.log().Infow("billing: credit granted", "organization", org, "amount", req.Amount, "days", req.Days, "by", by)
	if err := s.settleAccount(ctx, org); err != nil {
		return nil, err
	}
	return s.Wallet(ctx, org)
}

// Adjust : corrige l'argent d'un client (positif : crédité ; négatif : débité).
func (s *Service) Adjust(ctx context.Context, org uuid.UUID, req domain.AdjustmentRequest, by string) (*domain.Wallet, error) {
	why, err := reason(req.Reason)
	if err != nil {
		return nil, err
	}
	if req.Amount == 0 {
		return nil, domain.ErrInvalidAdminAmount
	}
	if err := s.requireOrg(ctx, org); err != nil {
		return nil, err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	amount := domain.FromUnits(req.Amount)
	id := uuid.New()
	t, err := domain.NewTransaction(org, domain.TxAdjustment, domain.AdjustmentReference(id), acc.Currency, why, by,
		map[string]domain.Milli{domain.CashAccount(org): amount, domain.AccountAdjustments: -amount}, s.now())
	if err != nil {
		return nil, err
	}
	if _, err := s.Ledger.Post(ctx, t); err != nil {
		return nil, err
	}
	s.log().Infow("billing: balance adjusted", "organization", org, "amount", req.Amount, "by", by)
	if err := s.settleAccount(ctx, org); err != nil {
		return nil, err
	}
	s.checkBalance(ctx, org)
	return s.Wallet(ctx, org)
}

// SetCharging : active (ou suspend) la facturation réelle d'une organisation.
// Le crédit d'essai n'est plus offert ici : il l'est à la première recharge
// réussie, une fois par numéro Mobile Money (anti-abus). Suspendre la
// facturation d'une organisation arrêtée faute de solde la relance.
func (s *Service) SetCharging(ctx context.Context, org uuid.UUID, enabled bool, by string) (*domain.Wallet, error) {
	if err := s.requireOrg(ctx, org); err != nil {
		return nil, err
	}
	now := s.now()
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	if err := s.Ledger.SetCharging(ctx, org, enabled, now); err != nil {
		return nil, err
	}
	s.log().Infow("billing: charging changed", "organization", org, "enabled", enabled, "by", by)
	if !enabled && s.Phase4 != nil {
		if acc.SuspendedAt != nil {
			if err := s.resume(ctx, org); err != nil {
				return nil, err
			}
		}
		if err := s.Phase4.SetGrace(ctx, org, nil); err != nil {
			return nil, err
		}
	}
	return s.Wallet(ctx, org)
}

// Revenue : recettes par jour et par monnaie, recharges en attente ou ratées.
func (s *Service) Revenue(ctx context.Context, days int) (*domain.RevenueResponse, error) {
	if days <= 0 || days > 366 {
		days = 30
	}
	n := s.now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	list, err := s.Admin.Revenue(ctx, from)
	if err != nil {
		return nil, err
	}
	pending, failed, err := s.Admin.TopupCounts(ctx, from)
	if err != nil {
		return nil, err
	}
	return &domain.RevenueResponse{Days: list, PendingTopups: pending, FailedTopups: failed}, nil
}
