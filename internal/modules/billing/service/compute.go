package service

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// VPS revendus (docs/modules/compute.md) : un mois est débité du solde payé
// (jamais des crédits offerts ; postpayé : dans le plafond), dans la
// transaction SQL de l'appelant (commande, demande au fournisseur) : tout
// réussit ou rien. Idempotent par référence.

// ChargeComputeTx : débite un mois de VPS dans tx ; faux si cette référence
// était déjà débitée. Appeler AfterComputeCharge une fois tx validée.
func (s *Service) ChargeComputeTx(ctx context.Context, tx *gorm.DB, org uuid.UUID, amount domain.Milli, reference, description, by string) (bool, error) {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return false, err
	}
	var allowance domain.Milli
	if acc.Postpaid {
		c, err := s.catalog(ctx, org)
		if err != nil {
			return false, err
		}
		allowance = postpaidTerms(acc, c).CreditLimit
	}
	return repository.NewLedger(tx.WithContext(ctx)).DebitCash(ctx, org, amount, allowance, domain.TxCompute, reference,
		acc.Currency, description, by, domain.AccountCompute, s.now())
}

// AfterComputeCharge : alertes de solde après un débit validé.
func (s *Service) AfterComputeCharge(ctx context.Context, org uuid.UUID) { s.checkBalance(ctx, org) }

// RefundCompute : rembourse un mois de VPS non livré (idempotent par référence).
func (s *Service) RefundCompute(ctx context.Context, org uuid.UUID, amount domain.Milli, reference, description string) error {
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return err
	}
	t, err := domain.NewTransaction(org, domain.TxComputeRefund, reference, acc.Currency, description, "billing",
		map[string]domain.Milli{domain.CashAccount(org): amount, domain.AccountCompute: -amount}, s.now())
	if err != nil {
		return err
	}
	if _, err := s.Ledger.Post(ctx, t); err != nil {
		return err
	}
	return s.settleAccount(ctx, org)
}
