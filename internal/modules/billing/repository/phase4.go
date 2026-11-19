package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Phase 4 : grâce et suspension, documents, budgets, crédit d'essai.
type Phase4 interface {
	SetGrace(ctx context.Context, org uuid.UUID, at *time.Time) error
	SetSuspended(ctx context.Context, org uuid.UUID, at *time.Time) error
	SetProfile(ctx context.Context, org uuid.UUID, p domain.Profile) error

	// RecordStopped : conteneurs arrêtés par la facturation (à relancer à la reprise).
	RecordStopped(ctx context.Context, org, project, workload uuid.UUID, reason string, at time.Time) error
	// Stopped : conteneurs arrêtés pour une raison (project nil : toute l'organisation).
	Stopped(ctx context.Context, org uuid.UUID, project *uuid.UUID, reason string) ([]uuid.UUID, error)
	ClearStopped(ctx context.Context, workload uuid.UUID) error

	// ClaimTrial : true si ce numéro n'a jamais donné de crédit d'essai, ni
	// cette organisation (le droit est alors pris).
	ClaimTrial(ctx context.Context, phone string, org uuid.UUID, at time.Time) (bool, error)

	// IssueDocument : prend le numéro suivant (sans trou) et écrit le document
	// rendu par render ; false si le document existe déjà (même facture ou
	// même reçu). vat : écriture de TVA à poster dans la même transaction.
	IssueDocument(ctx context.Context, d *domain.Document, prefix string, render func(number string) ([]byte, error), vat *domain.Transaction) (bool, error)
	Documents(ctx context.Context, org uuid.UUID, limit int) ([]domain.Document, error)
	Document(ctx context.Context, org, id uuid.UUID) (*domain.Document, error)
	InvoiceExists(ctx context.Context, org uuid.UUID, period string) (bool, error)
	// UsageDebits : débits d'usage d'une période, part crédits et part argent (positifs).
	UsageDebits(ctx context.Context, org uuid.UUID, period string) (credit, cash domain.Milli, err error)
	// InvoiceableOrgs : organisations débitées sur la période.
	InvoiceableOrgs(ctx context.Context, period string) ([]uuid.UUID, error)
	// TopupsWithoutReceipt : recharges réussies sans reçu.
	TopupsWithoutReceipt(ctx context.Context, limit int) ([]domain.Topup, error)

	Budget(ctx context.Context, project uuid.UUID) (*BudgetRow, error)
	SetBudget(ctx context.Context, b BudgetRow) error
	DeleteBudget(ctx context.Context, project uuid.UUID) error
	Budgets(ctx context.Context) ([]BudgetRow, error)
	SetBudgetAlert(ctx context.Context, project uuid.UUID, level int, period string) error
}

// BudgetRow : budget d'un projet.
type BudgetRow struct {
	ProjectID      uuid.UUID
	OrganizationID uuid.UUID
	MonthlyLimit   domain.Milli
	Stop           bool
	AlertLevel     int
	Period         string
	UpdatedBy      string
}

type phase4Store struct{ db *gorm.DB }

// NewPhase4 : stockage de la phase 4.
func NewPhase4(db *gorm.DB) Phase4 { return &phase4Store{db: db} }

func (s *phase4Store) upsertAccount(ctx context.Context, org uuid.UUID, col string, val any) error {
	return s.db.WithContext(ctx).Exec(`INSERT INTO billing_accounts (organization_id, `+col+`) VALUES (?, ?)
		ON CONFLICT (organization_id) DO UPDATE SET `+col+` = excluded.`+col+`, updated_at = NOW()`, org, val).Error
}

func (s *phase4Store) SetGrace(ctx context.Context, org uuid.UUID, at *time.Time) error {
	return s.upsertAccount(ctx, org, "grace_started_at", at)
}

// SetSuspended : à la reprise (at nil), le compte à rebours de la
// suppression des données repart de zéro.
func (s *phase4Store) SetSuspended(ctx context.Context, org uuid.UUID, at *time.Time) error {
	if at != nil {
		return s.upsertAccount(ctx, org, "suspended_at", at)
	}
	return s.db.WithContext(ctx).Exec(`INSERT INTO billing_accounts (organization_id) VALUES (?)
		ON CONFLICT (organization_id) DO UPDATE SET suspended_at = NULL, purge_stage = 0, purge_noticed_at = NULL, data_purged_at = NULL, updated_at = NOW()`,
		org).Error
}

func (s *phase4Store) SetProfile(ctx context.Context, org uuid.UUID, p domain.Profile) error {
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO billing_accounts (organization_id, legal_name, tax_id, address, country) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (organization_id) DO UPDATE SET legal_name = excluded.legal_name, tax_id = excluded.tax_id,
			address = excluded.address, country = excluded.country, updated_at = NOW()`,
		org, p.LegalName, p.TaxID, p.Address, p.Country).Error
}

func (s *phase4Store) RecordStopped(ctx context.Context, org, project, workload uuid.UUID, reason string, at time.Time) error {
	return s.db.WithContext(ctx).Exec(`INSERT INTO billing_stopped_workloads (workload_id, organization_id, project_id, reason, stopped_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (workload_id) DO NOTHING`, workload, org, project, reason, at).Error
}

func (s *phase4Store) Stopped(ctx context.Context, org uuid.UUID, project *uuid.UUID, reason string) ([]uuid.UUID, error) {
	q := s.db.WithContext(ctx).Table("billing_stopped_workloads").Where("organization_id = ? AND reason = ?", org, reason)
	if project != nil {
		q = q.Where("project_id = ?", *project)
	}
	var ids []uuid.UUID
	return ids, q.Order("stopped_at").Pluck("workload_id", &ids).Error
}

func (s *phase4Store) ClearStopped(ctx context.Context, workload uuid.UUID) error {
	return s.db.WithContext(ctx).Exec(`DELETE FROM billing_stopped_workloads WHERE workload_id = ?`, workload).Error
}

func (s *phase4Store) ClaimTrial(ctx context.Context, phone string, org uuid.UUID, at time.Time) (bool, error) {
	res := s.db.WithContext(ctx).Exec(`INSERT INTO billing_trials (phone, organization_id, granted_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, phone, org, at)
	return res.RowsAffected > 0, res.Error
}

func (s *phase4Store) IssueDocument(ctx context.Context, d *domain.Document, prefix string,
	render func(number string) ([]byte, error), vat *domain.Transaction) (bool, error) {
	issued := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrg(tx, d.OrganizationID); err != nil {
			return err
		}
		var exists int64
		q := tx.Model(&domain.Document{}).Where("organization_id = ? AND kind = ?", d.OrganizationID, d.Kind)
		if d.Kind == domain.DocInvoice {
			q = q.Where("period = ?", d.Period)
		} else {
			q = q.Where("topup_id = ?", d.TopupID)
		}
		if err := q.Count(&exists).Error; err != nil || exists > 0 {
			return err
		}
		// Compteur pris sous verrou de ligne : la transaction annulée rend
		// le numéro, il n'y a jamais de trou.
		var n int
		year := d.IssuedAt.UTC().Year()
		if err := tx.Raw(`INSERT INTO billing_number_counters (prefix, year, last) VALUES (?, ?, 1)
			ON CONFLICT (prefix, year) DO UPDATE SET last = billing_number_counters.last + 1 RETURNING last`, prefix, year).
			Scan(&n).Error; err != nil {
			return err
		}
		d.Number = domain.DocumentNumber(prefix, year, n)
		pdf, err := render(d.Number)
		if err != nil {
			return err
		}
		d.PDF = pdf
		if err := tx.Create(d).Error; err != nil {
			return err
		}
		if vat != nil {
			if _, err := post(tx, vat); err != nil {
				return err
			}
		}
		issued = true
		return nil
	})
	return issued, err
}

func (s *phase4Store) Documents(ctx context.Context, org uuid.UUID, limit int) ([]domain.Document, error) {
	out := []domain.Document{}
	err := s.db.WithContext(ctx).Omit("pdf", "data").Where("organization_id = ?", org).
		Order("issued_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *phase4Store) Document(ctx context.Context, org, id uuid.UUID) (*domain.Document, error) {
	var d domain.Document
	err := s.db.WithContext(ctx).Where("organization_id = ? AND id = ?", org, id).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDocumentNotFound
	}
	return &d, err
}

func (s *phase4Store) InvoiceExists(ctx context.Context, org uuid.UUID, period string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.Document{}).
		Where("organization_id = ? AND kind = ? AND period = ?", org, domain.DocInvoice, period).Count(&n).Error
	return n > 0, err
}

func (s *phase4Store) UsageDebits(ctx context.Context, org uuid.UUID, period string) (domain.Milli, domain.Milli, error) {
	var r struct{ Credit, Cash int64 }
	err := s.db.WithContext(ctx).Raw(`SELECT
			COALESCE(-SUM(e.amount) FILTER (WHERE e.account = ?), 0) AS credit,
			COALESCE(-SUM(e.amount) FILTER (WHERE e.account = ?), 0) AS cash
		FROM billing_ledger_entries e JOIN billing_transactions t ON t.id = e.transaction_id
		WHERE t.organization_id = ? AND t.kind = ? AND t.period = ?`,
		domain.CreditAccount(org), domain.CashAccount(org), org, domain.TxUsage, period).Scan(&r).Error
	return domain.Milli(r.Credit), domain.Milli(r.Cash), err
}

func (s *phase4Store) InvoiceableOrgs(ctx context.Context, period string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`SELECT DISTINCT organization_id FROM billing_transactions
		WHERE kind = ? AND period = ? AND organization_id IS NOT NULL ORDER BY organization_id`, domain.TxUsage, period).Scan(&ids).Error
	return ids, err
}

func (s *phase4Store) TopupsWithoutReceipt(ctx context.Context, limit int) ([]domain.Topup, error) {
	var out []domain.Topup
	err := s.db.WithContext(ctx).Raw(`SELECT t.* FROM billing_topups t
		LEFT JOIN billing_documents d ON d.topup_id = t.id AND d.kind = ?
		WHERE t.status = ? AND d.id IS NULL ORDER BY t.settled_at LIMIT ?`, domain.DocReceipt, domain.TopupSucceeded, limit).
		Scan(&out).Error
	return out, err
}

type budgetDB struct {
	ProjectID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"type:uuid"`
	MonthlyLimit   int64
	Stop           bool
	AlertLevel     int
	Period         string
	UpdatedBy      string
	UpdatedAt      time.Time
}

func (budgetDB) TableName() string { return "billing_project_budgets" }

func (b budgetDB) row() BudgetRow {
	return BudgetRow{ProjectID: b.ProjectID, OrganizationID: b.OrganizationID, MonthlyLimit: domain.Milli(b.MonthlyLimit),
		Stop: b.Stop, AlertLevel: b.AlertLevel, Period: b.Period, UpdatedBy: b.UpdatedBy}
}

func (s *phase4Store) Budget(ctx context.Context, project uuid.UUID) (*BudgetRow, error) {
	var b budgetDB
	err := s.db.WithContext(ctx).First(&b, "project_id = ?", project).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := b.row()
	return &r, nil
}

func (s *phase4Store) SetBudget(ctx context.Context, b BudgetRow) error {
	row := budgetDB{ProjectID: b.ProjectID, OrganizationID: b.OrganizationID, MonthlyLimit: int64(b.MonthlyLimit), Stop: b.Stop,
		AlertLevel: b.AlertLevel, Period: b.Period, UpdatedBy: b.UpdatedBy, UpdatedAt: time.Now()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "project_id"}},
		// Les seuils déjà prévenus restent : modifier le budget ne renvoie pas l'alerte du mois.
		DoUpdates: clause.AssignmentColumns([]string{"monthly_limit", "stop", "updated_by", "updated_at"}),
	}).Create(&row).Error
}

func (s *phase4Store) DeleteBudget(ctx context.Context, project uuid.UUID) error {
	return s.db.WithContext(ctx).Exec(`DELETE FROM billing_project_budgets WHERE project_id = ?`, project).Error
}

func (s *phase4Store) Budgets(ctx context.Context) ([]BudgetRow, error) {
	var rows []budgetDB
	if err := s.db.WithContext(ctx).Order("project_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BudgetRow, len(rows))
	for i, r := range rows {
		out[i] = r.row()
	}
	return out, nil
}

func (s *phase4Store) SetBudgetAlert(ctx context.Context, project uuid.UUID, level int, period string) error {
	return s.db.WithContext(ctx).Exec(`UPDATE billing_project_budgets SET alert_level = ?, period = ? WHERE project_id = ?`,
		level, period, project).Error
}
