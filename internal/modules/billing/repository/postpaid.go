package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Postpaid : réglages et créances du postpayé.
type Postpaid interface {
	// SetPostpaid : postpayé (avec plafond et échéance propres, nil : grille)
	// ou retour au prépayé ; le postpayé active aussi la facturation réelle.
	SetPostpaid(ctx context.Context, org uuid.UUID, enabled bool, limit *domain.Milli, terms *int, now time.Time) error
	PostpaidOrgs(ctx context.Context) ([]uuid.UUID, error)
	// Invoices : factures mensuelles de l'organisation.
	Invoices(ctx context.Context, org uuid.UUID) ([]domain.Invoice, error)
	// UninvoicedCashUsage : usage payé en argent des mois pas encore facturés.
	UninvoicedCashUsage(ctx context.Context, org uuid.UUID) (domain.Milli, error)
	// MarkReminded : true si la relance de cette facture n'était pas encore partie.
	MarkReminded(ctx context.Context, document uuid.UUID, at time.Time) (bool, error)
}

type postpaidStore struct{ db *gorm.DB }

// NewPostpaid : stockage du postpayé.
func NewPostpaid(db *gorm.DB) Postpaid { return &postpaidStore{db: db} }

func (s *postpaidStore) SetPostpaid(ctx context.Context, org uuid.UUID, enabled bool, limit *domain.Milli, terms *int, now time.Time) error {
	var l *int64
	if limit != nil {
		v := int64(*limit)
		l = &v
	}
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO billing_accounts (organization_id, postpaid, credit_limit, terms_days, charging, charging_since)
		VALUES (?, ?, ?, ?, ?, CASE WHEN ? THEN ?::timestamptz END)
		ON CONFLICT (organization_id) DO UPDATE SET postpaid = excluded.postpaid,
			credit_limit = excluded.credit_limit, terms_days = excluded.terms_days,
			charging = billing_accounts.charging OR excluded.charging,
			charging_since = COALESCE(billing_accounts.charging_since, excluded.charging_since),
			updated_at = NOW()`, org, enabled, l, terms, enabled, enabled, now).Error
}

func (s *postpaidStore) PostpaidOrgs(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`SELECT organization_id FROM billing_accounts WHERE postpaid ORDER BY organization_id`).Scan(&ids).Error
	return ids, err
}

func (s *postpaidStore) Invoices(ctx context.Context, org uuid.UUID) ([]domain.Invoice, error) {
	var rows []struct {
		ID       uuid.UUID
		Number   string
		Period   string
		Total    int64
		IssuedAt time.Time
		DueAt    *time.Time
	}
	if err := s.db.WithContext(ctx).Raw(`SELECT id, number, period, total, issued_at, due_at FROM billing_documents
		WHERE organization_id = ? AND kind = ? ORDER BY issued_at`, org, domain.DocInvoice).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Invoice, len(rows))
	for i, r := range rows {
		out[i] = domain.Invoice{ID: r.ID, Number: r.Number, Period: r.Period, Total: domain.Milli(r.Total), IssuedAt: r.IssuedAt, DueAt: r.DueAt}
	}
	return out, nil
}

func (s *postpaidStore) UninvoicedCashUsage(ctx context.Context, org uuid.UUID) (domain.Milli, error) {
	var sum int64
	err := s.db.WithContext(ctx).Raw(`SELECT COALESCE(-SUM(e.amount), 0) FROM billing_ledger_entries e
		JOIN billing_transactions t ON t.id = e.transaction_id
		WHERE t.organization_id = ? AND t.kind = ? AND e.account = ?
		  AND NOT EXISTS (SELECT 1 FROM billing_documents d WHERE d.organization_id = t.organization_id
		                  AND d.kind = ? AND d.period = t.period)`,
		org, domain.TxUsage, domain.CashAccount(org), domain.DocInvoice).Scan(&sum).Error
	return domain.Milli(sum), err
}

func (s *postpaidStore) MarkReminded(ctx context.Context, document uuid.UUID, at time.Time) (bool, error) {
	res := s.db.WithContext(ctx).Exec(`INSERT INTO billing_dunning (document_id, reminded_at) VALUES (?, ?)
		ON CONFLICT (document_id) DO UPDATE SET reminded_at = excluded.reminded_at WHERE billing_dunning.reminded_at IS NULL`, document, at)
	return res.RowsAffected > 0, res.Error
}
