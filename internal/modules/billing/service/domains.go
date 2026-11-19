package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Ventes de noms de domaine : le module des domaines débite et rembourse le
// client par ces méthodes. Un achat se paie avec l'argent payé (jamais les
// crédits offerts : il coûte de l'argent réel chez le registraire).

// AccountCatalog : grille en vigueur de l'organisation (prix des domaines).
func (s *Service) AccountCatalog(ctx context.Context, org uuid.UUID) (domain.Catalog, error) {
	return s.catalog(ctx, org)
}

// ChargePurchase : débite un achat (idempotent par référence). Le postpayé
// peut l'imputer sur son plafond.
func (s *Service) ChargePurchase(ctx context.Context, org uuid.UUID, amount domain.Milli, reference, description, by string) error {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return err
	}
	var allowance domain.Milli
	if acc.Postpaid {
		c, err := s.catalog(ctx, org)
		if err != nil {
			return err
		}
		allowance = postpaidTerms(acc, c).CreditLimit
	}
	if _, err := s.Ledger.DebitCash(ctx, org, amount, allowance, domain.TxDomain, reference, acc.Currency, description, by,
		domain.AccountDomains, s.now()); err != nil {
		return err
	}
	s.checkBalance(ctx, org)
	return nil
}

// RefundPurchase : rembourse un achat non obtenu (idempotent par référence).
func (s *Service) RefundPurchase(ctx context.Context, org uuid.UUID, amount domain.Milli, reference, description string) error {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return err
	}
	t, err := domain.NewTransaction(org, domain.TxDomainRefund, reference, acc.Currency, description, "billing",
		map[string]domain.Milli{domain.CashAccount(org): amount, domain.AccountDomains: -amount}, s.now())
	if err != nil {
		return err
	}
	if _, err := s.Ledger.Post(ctx, t); err != nil {
		return err
	}
	return s.settleAccount(ctx, org)
}
