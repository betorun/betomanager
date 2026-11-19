package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Postpayé (ADR 0012, phase 5) : la dette est tolérée jusqu'au plafond de
// crédit ; chaque facture est à payer avant son échéance ; impayée, relance
// puis suspension douce, levée dès que la dette redescend.

// InvoiceReminder : relance e-mail d'une facture impayée (owners de l'organisation).
type InvoiceReminder interface {
	InvoiceOverdue(ctx context.Context, org uuid.UUID, number string, unpaid int64, dueAt time.Time, currency string)
}

// postpaidTerms : réglages du compte, sinon défauts de la grille.
func postpaidTerms(acc repository.Account, c domain.Catalog) domain.PostpaidTerms {
	t := domain.PostpaidTerms{CreditLimit: c.PostpaidCreditLimit, TermsDays: c.PostpaidTermsDays,
		ReminderDays: c.PostpaidReminderDays, SuspendDays: c.PostpaidSuspendDays}
	if acc.CreditLimit != nil {
		t.CreditLimit = *acc.CreditLimit
	}
	if acc.TermsDays != nil {
		t.TermsDays = *acc.TermsDays
	}
	return t
}

// receivables : factures non soldées de l'organisation.
func (s *Service) receivables(ctx context.Context, org uuid.UUID, cash domain.Milli) ([]domain.ReceivableView, error) {
	invoices, err := s.Postpaid.Invoices(ctx, org)
	if err != nil {
		return nil, err
	}
	uninvoiced, err := s.Postpaid.UninvoicedCashUsage(ctx, org)
	if err != nil {
		return nil, err
	}
	return domain.Receivables(invoices, domain.InvoicedDebt(cash, uninvoiced), s.now()), nil
}

// postpaidView : état du postpayé pour le wallet.
func (s *Service) postpaidView(ctx context.Context, org uuid.UUID, acc repository.Account, c domain.Catalog, cash, credit domain.Milli) (*domain.PostpaidView, error) {
	t := postpaidTerms(acc, c)
	rec, err := s.receivables(ctx, org, cash)
	if err != nil {
		return nil, err
	}
	debt := max(0, -(cash + credit))
	return &domain.PostpaidView{CreditLimit: t.CreditLimit.Units(), TermsDays: t.TermsDays, Outstanding: debt.Units(),
		Available: (t.CreditLimit - debt).Units(), Invoices: rec}, nil
}

// overdueTooLong : une facture impayée depuis plus que le délai de suspension.
func overdueTooLong(rec []domain.ReceivableView, t domain.PostpaidTerms) bool {
	if t.SuspendDays <= 0 {
		return false
	}
	for _, r := range rec {
		if r.DaysOverdue >= t.SuspendDays {
			return true
		}
	}
	return false
}

// postpaidState : solde effectif (plafond compris) et retard bloquant.
func (s *Service) postpaidState(ctx context.Context, org uuid.UUID, acc repository.Account, balance, cash domain.Milli) (domain.Milli, bool, error) {
	if !acc.Postpaid || s.Postpaid == nil {
		return balance, false, nil
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return 0, false, err
	}
	t := postpaidTerms(acc, c)
	rec, err := s.receivables(ctx, org, cash)
	if err != nil {
		return 0, false, err
	}
	return balance + t.CreditLimit, overdueTooLong(rec, t), nil
}

// RunPostpaid : relances et suspensions du postpayé (worker ; exposé pour
// les tests).
func (s *Service) RunPostpaid(ctx context.Context) { s.dunning(ctx) }

// dunning : relance une fois chaque facture en retard de plus du délai de
// relance (worker). La suspension, elle, passe par settleAccount.
func (s *Service) dunning(ctx context.Context) {
	if s.Postpaid == nil {
		return
	}
	orgs, err := s.Postpaid.PostpaidOrgs(ctx)
	if err != nil {
		s.log().Warnw("billing: postpaid organizations lookup failed", "error", err)
		return
	}
	for _, org := range orgs {
		acc, err := s.Ledger.Account(ctx, org)
		if err != nil {
			continue
		}
		c, err := s.catalog(ctx, org)
		if err != nil {
			continue
		}
		t := postpaidTerms(acc, c)
		cash, _, err := s.Ledger.Balances(ctx, org)
		if err != nil {
			continue
		}
		rec, err := s.receivables(ctx, org, cash)
		if err != nil {
			continue
		}
		for _, r := range rec {
			if t.ReminderDays <= 0 || r.DaysOverdue < t.ReminderDays || r.DueAt == nil {
				continue
			}
			id, _ := uuid.Parse(r.DocumentID)
			first, err := s.Postpaid.MarkReminded(ctx, id, s.now())
			if err != nil || !first {
				continue
			}
			if s.Reminders != nil {
				s.Reminders.InvoiceOverdue(ctx, org, r.Number, r.Unpaid, *r.DueAt, c.Currency)
			}
		}
		if err := s.settleAccount(ctx, org); err != nil {
			s.log().Warnw("billing: postpaid settlement failed", "organization", org, "error", err)
		}
	}
}

// SetPostpaid : passe une organisation en postpayé (plafond et échéance
// propres, sinon ceux de la grille) ou la ramène au prépayé (équipe).
func (s *Service) SetPostpaid(ctx context.Context, org uuid.UUID, req domain.PostpaidRequest, by string) (*domain.Wallet, error) {
	if err := s.requireOrg(ctx, org); err != nil {
		return nil, err
	}
	var limit *domain.Milli
	if req.CreditLimit != nil {
		if *req.CreditLimit < 0 {
			return nil, domain.ErrInvalidAdminAmount
		}
		l := domain.FromUnits(*req.CreditLimit)
		limit = &l
	}
	if req.TermsDays != nil && (*req.TermsDays < 0 || *req.TermsDays > 120) {
		return nil, domain.ErrInvalidAdminAmount
	}
	if err := s.Postpaid.SetPostpaid(ctx, org, req.Enabled, limit, req.TermsDays, s.now()); err != nil {
		return nil, err
	}
	s.log().Infow("billing: postpaid changed", "organization", org, "enabled", req.Enabled, "by", by)
	if err := s.settleAccount(ctx, org); err != nil {
		return nil, err
	}
	return s.Wallet(ctx, org)
}

// RecordPayment : virement reçu (équipe). Réduit la dette ; les factures sont
// soldées de la plus ancienne à la plus récente ; une suspension se lève.
func (s *Service) RecordPayment(ctx context.Context, org uuid.UUID, req domain.PaymentRequest, by string) (*domain.Wallet, error) {
	ref := strings.TrimSpace(req.Reference)
	if req.Amount <= 0 || ref == "" {
		return nil, domain.ErrInvalidPayment
	}
	if len(ref) > 200 {
		ref = ref[:200]
	}
	if err := s.requireOrg(ctx, org); err != nil {
		return nil, err
	}
	acc, err := s.Ledger.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	amount := domain.FromUnits(req.Amount)
	t, err := domain.NewTransaction(org, domain.TxPayment, domain.PaymentReference(uuid.New()), acc.Currency, ref, by,
		map[string]domain.Milli{domain.CashAccount(org): amount, domain.AccountBank: -amount}, s.now())
	if err != nil {
		return nil, err
	}
	if _, err := s.Ledger.Post(ctx, t); err != nil {
		return nil, err
	}
	s.log().Infow("billing: payment recorded", "organization", org, "amount", req.Amount, "by", by)
	if err := s.settleAccount(ctx, org); err != nil {
		return nil, err
	}
	s.checkBalance(ctx, org)
	return s.Wallet(ctx, org)
}
