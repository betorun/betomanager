package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Ledger : grand livre, crédits, recharges et comptes (phase 3). Tout
// mouvement qui touche le solde d'un client passe sous un verrou propre à son
// organisation (verrou consultatif de Postgres, le temps de la transaction
// SQL) : deux workers ne débitent jamais la même heure deux fois, et un
// débit ne croise jamais une recharge ou un crédit.
type Ledger interface {
	// Account : réglages du compte (défauts sans ligne).
	Account(ctx context.Context, org uuid.UUID) (Account, error)
	SetCharging(ctx context.Context, org uuid.UUID, enabled bool, now time.Time) error
	SetAlertLevel(ctx context.Context, org uuid.UUID, level int) error
	// ChargingOrgs : organisations dont la facturation réelle est active.
	ChargingOrgs(ctx context.Context) ([]uuid.UUID, error)

	// Post : écrit une transaction équilibrée ; false si sa référence existe déjà.
	Post(ctx context.Context, tx *domain.Transaction) (bool, error)
	// Balances : argent et crédits du client.
	Balances(ctx context.Context, org uuid.UUID) (cash, credit domain.Milli, err error)
	// ActiveCredits : crédits encore valables, le plus proche de l'expiration d'abord.
	ActiveCredits(ctx context.Context, org uuid.UUID, now time.Time) ([]domain.CreditGrant, error)
	// ChargeUsage : débite ce qui manque pour que les débits d'usage de la
	// période atteignent target (crédits d'abord, puis argent). Rien si c'est
	// déjà atteint ; idempotent par référence. Rend le montant débité.
	ChargeUsage(ctx context.Context, org uuid.UUID, period string, target domain.Milli, reference, currency, description string, now time.Time) (domain.Milli, error)
	// GrantCredit : crédit offert (transaction et lot) ; false si la référence existe.
	GrantCredit(ctx context.Context, g *domain.CreditGrant, reference, by string, now time.Time) (bool, error)
	// ExpireCredits : retire le reste des crédits expirés ; rend leur nombre.
	ExpireCredits(ctx context.Context, now time.Time) (int, error)
	// Transactions : historique, du plus récent au plus ancien.
	Transactions(ctx context.Context, org uuid.UUID, before *Cursor, limit int) ([]domain.TransactionView, error)

	CreateTopup(ctx context.Context, t *domain.Topup) error
	SetTopupPayment(ctx context.Context, id uuid.UUID, paymentID, environment string) error
	Topup(ctx context.Context, org, id uuid.UUID) (*domain.Topup, error)
	TopupByPayment(ctx context.Context, paymentID string) (*domain.Topup, error)
	RecentTopups(ctx context.Context, org uuid.UUID, limit int) ([]domain.Topup, error)
	HasPendingTopup(ctx context.Context, org uuid.UUID, since time.Time) (bool, error)
	PendingTopups(ctx context.Context, olderThan time.Time, limit int) ([]domain.Topup, error)
	// SettleTopup : issue d'une recharge. Réussie : l'argent est crédité
	// (une seule fois, même relancée). Rend la recharge à jour.
	SettleTopup(ctx context.Context, id uuid.UUID, status, failure string, now time.Time) (*domain.Topup, error)
}

// Account : réglages d'un compte de facturation.
type Account struct {
	Currency       string
	Charging       bool
	ChargingSince  *time.Time
	AlertLevel     int
	GraceStartedAt *time.Time
	SuspendedAt    *time.Time
	Profile        domain.Profile
	// Postpayé : plafond et échéance propres au compte (nil : grille).
	Postpaid    bool
	CreditLimit *domain.Milli
	TermsDays   *int
}

// Cursor : position dans l'historique.
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

// ParseCursor : "RFC3339Nano|uuid".
func ParseCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	at, id, ok := strings.Cut(s, "|")
	if !ok {
		return nil, errors.New("invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &Cursor{At: t, ID: u}, nil
}

// String : forme passée au client.
func (c Cursor) String() string { return c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String() }

type ledgerStore struct{ db *gorm.DB }

// NewLedger : grand livre en base.
func NewLedger(db *gorm.DB) Ledger { return &ledgerStore{db: db} }

// lockOrg : verrou de l'organisation jusqu'à la fin de la transaction SQL.
func lockOrg(tx *gorm.DB, org uuid.UUID) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, "billing:"+org.String()).Error
}

func (s *ledgerStore) Account(ctx context.Context, org uuid.UUID) (Account, error) {
	var rows []struct {
		Currency       string
		Charging       bool
		ChargingSince  *time.Time
		AlertLevel     int
		GraceStartedAt *time.Time
		SuspendedAt    *time.Time
		LegalName      string
		TaxID          string
		Address        string
		Country        string
		Postpaid       bool
		CreditLimit    *int64
		TermsDays      *int
	}
	err := s.db.WithContext(ctx).Raw(`SELECT currency, charging, charging_since, alert_level, grace_started_at, suspended_at,
		legal_name, tax_id, address, country, postpaid, credit_limit, terms_days FROM billing_accounts WHERE organization_id = ?`, org).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return Account{Currency: domain.DefaultCurrency}, err
	}
	r := rows[0]
	acc := Account{Currency: r.Currency, Charging: r.Charging, ChargingSince: r.ChargingSince, AlertLevel: r.AlertLevel,
		GraceStartedAt: r.GraceStartedAt, SuspendedAt: r.SuspendedAt,
		Profile:  domain.Profile{LegalName: r.LegalName, TaxID: r.TaxID, Address: r.Address, Country: r.Country},
		Postpaid: r.Postpaid, TermsDays: r.TermsDays}
	if r.CreditLimit != nil {
		l := domain.Milli(*r.CreditLimit)
		acc.CreditLimit = &l
	}
	return acc, nil
}

func (s *ledgerStore) SetCharging(ctx context.Context, org uuid.UUID, enabled bool, now time.Time) error {
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO billing_accounts (organization_id, charging, charging_since)
		VALUES (?, ?, CASE WHEN ? THEN ?::timestamptz END)
		ON CONFLICT (organization_id) DO UPDATE SET
			charging = excluded.charging,
			charging_since = CASE WHEN excluded.charging THEN COALESCE(billing_accounts.charging_since, excluded.charging_since) ELSE billing_accounts.charging_since END,
			updated_at = NOW()`, org, enabled, enabled, now).Error
}

func (s *ledgerStore) SetAlertLevel(ctx context.Context, org uuid.UUID, level int) error {
	return s.db.WithContext(ctx).Exec(`UPDATE billing_accounts SET alert_level = ?, updated_at = NOW() WHERE organization_id = ?`, level, org).Error
}

func (s *ledgerStore) ChargingOrgs(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`SELECT organization_id FROM billing_accounts WHERE charging ORDER BY organization_id`).Scan(&ids).Error
	return ids, err
}

// post : écrit la transaction dans tx ; false si la référence existe déjà.
func post(tx *gorm.DB, t *domain.Transaction) (bool, error) {
	entries := t.Entries
	t.Entries = nil
	res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "reference"}}, DoNothing: true}).Create(t)
	t.Entries = entries
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	if err := tx.Create(&entries).Error; err != nil {
		return false, err
	}
	return true, nil
}

func (s *ledgerStore) Post(ctx context.Context, t *domain.Transaction) (bool, error) {
	var written bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if t.OrganizationID != nil {
			if err := lockOrg(tx, *t.OrganizationID); err != nil {
				return err
			}
		}
		var err error
		written, err = post(tx, t)
		return err
	})
	return written, err
}

func balances(tx *gorm.DB, org uuid.UUID) (cash, credit domain.Milli, err error) {
	var r struct{ Cash, Credit int64 }
	err = tx.Raw(`SELECT
			COALESCE(SUM(amount) FILTER (WHERE account = ?), 0) AS cash,
			COALESCE(SUM(amount) FILTER (WHERE account = ?), 0) AS credit
		FROM billing_ledger_entries WHERE account IN (?, ?)`,
		domain.CashAccount(org), domain.CreditAccount(org), domain.CashAccount(org), domain.CreditAccount(org)).Scan(&r).Error
	return domain.Milli(r.Cash), domain.Milli(r.Credit), err
}

func (s *ledgerStore) Balances(ctx context.Context, org uuid.UUID) (domain.Milli, domain.Milli, error) {
	return balances(s.db.WithContext(ctx), org)
}

func (s *ledgerStore) ActiveCredits(ctx context.Context, org uuid.UUID, now time.Time) ([]domain.CreditGrant, error) {
	var out []domain.CreditGrant
	err := s.db.WithContext(ctx).Where("organization_id = ? AND remaining > 0 AND expires_at > ?", org, now).
		Order("expires_at, created_at").Find(&out).Error
	return out, err
}

func (s *ledgerStore) ChargeUsage(ctx context.Context, org uuid.UUID, period string, target domain.Milli,
	reference, currency, description string, now time.Time) (domain.Milli, error) {
	var charged domain.Milli
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrg(tx, org); err != nil {
			return err
		}
		var already int64
		if err := tx.Raw(`SELECT COALESCE(-SUM(e.amount), 0) FROM billing_ledger_entries e
			JOIN billing_transactions t ON t.id = e.transaction_id
			WHERE t.organization_id = ? AND t.kind = ? AND t.period = ? AND e.account IN (?, ?)`,
			org, domain.TxUsage, period, domain.CashAccount(org), domain.CreditAccount(org)).Scan(&already).Error; err != nil {
			return err
		}
		delta := target - domain.Milli(already)
		if delta <= 0 {
			return nil
		}
		var grants []domain.CreditGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND remaining > 0 AND expires_at > ?", org, now).
			Order("expires_at, created_at").Find(&grants).Error; err != nil {
			return err
		}
		lots := make([]domain.CreditLot, len(grants))
		for i, g := range grants {
			lots[i] = domain.CreditLot{ID: g.ID, Remaining: g.Remaining, ExpiresAt: g.ExpiresAt}
		}
		alloc := domain.Allocate(delta, lots, now)
		t, err := domain.NewTransaction(org, domain.TxUsage, reference, currency, description, "billing", domain.UsageEntries(org, alloc), now)
		if err != nil {
			return err
		}
		t.Period = period
		written, err := post(tx, t)
		if err != nil || !written {
			return err
		}
		for id, take := range alloc.FromCredits {
			if err := tx.Exec(`UPDATE billing_credit_grants SET remaining = remaining - ? WHERE id = ?`, take, id).Error; err != nil {
				return err
			}
		}
		charged = delta
		return nil
	})
	return charged, err
}

func (s *ledgerStore) GrantCredit(ctx context.Context, g *domain.CreditGrant, reference, by string, now time.Time) (bool, error) {
	var written bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrg(tx, g.OrganizationID); err != nil {
			return err
		}
		t, err := domain.NewTransaction(g.OrganizationID, domain.TxCreditGrant, reference, g.Currency, g.Reason, by,
			map[string]domain.Milli{domain.CreditAccount(g.OrganizationID): g.Amount, domain.AccountPromotions: -g.Amount}, now)
		if err != nil {
			return err
		}
		if written, err = post(tx, t); err != nil || !written {
			return err
		}
		g.TransactionID, g.Remaining, g.CreatedBy, g.CreatedAt = t.ID, g.Amount, by, now
		return tx.Create(g).Error
	})
	return written, err
}

func (s *ledgerStore) ExpireCredits(ctx context.Context, now time.Time) (int, error) {
	var due []domain.CreditGrant
	if err := s.db.WithContext(ctx).Where("remaining > 0 AND expires_at <= ? AND expired_at IS NULL", now).
		Limit(500).Find(&due).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, g := range due {
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockOrg(tx, g.OrganizationID); err != nil {
				return err
			}
			var cur domain.CreditGrant
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cur, "id = ?", g.ID).Error; err != nil {
				return err
			}
			if cur.Remaining <= 0 || cur.ExpiredAt != nil {
				return nil
			}
			t, err := domain.NewTransaction(cur.OrganizationID, domain.TxCreditExpiry, domain.ExpiryReference(cur.ID), cur.Currency,
				cur.Reason, "billing", map[string]domain.Milli{domain.CreditAccount(cur.OrganizationID): -cur.Remaining, domain.AccountPromotions: cur.Remaining}, now)
			if err != nil {
				return err
			}
			if _, err := post(tx, t); err != nil {
				return err
			}
			return tx.Exec(`UPDATE billing_credit_grants SET remaining = 0, expired_at = ? WHERE id = ?`, now, cur.ID).Error
		})
		if err != nil {
			return n, fmt.Errorf("expire credit %s: %w", g.ID, err)
		}
		n++
	}
	return n, nil
}

type txRow struct {
	ID          uuid.UUID
	Kind        string
	Description string
	Period      string
	CreatedAt   time.Time
	Cash        int64
	Credit      int64
}

func (s *ledgerStore) Transactions(ctx context.Context, org uuid.UUID, before *Cursor, limit int) ([]domain.TransactionView, error) {
	q := `SELECT t.id, t.kind, t.description, t.period, t.created_at,
			COALESCE(SUM(e.amount) FILTER (WHERE e.account = @cash), 0) AS cash,
			COALESCE(SUM(e.amount) FILTER (WHERE e.account = @credit), 0) AS credit
		FROM billing_transactions t JOIN billing_ledger_entries e ON e.transaction_id = t.id
		WHERE t.organization_id = @org`
	args := map[string]any{"cash": domain.CashAccount(org), "credit": domain.CreditAccount(org), "org": org, "limit": limit}
	if before != nil {
		q += ` AND (t.created_at, t.id) < (@at, @id)`
		args["at"], args["id"] = before.At, before.ID
	}
	q += ` GROUP BY t.id ORDER BY t.created_at DESC, t.id DESC LIMIT @limit`
	var rows []txRow
	if err := s.db.WithContext(ctx).Raw(q, args).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.TransactionView, 0, len(rows))
	for _, r := range rows {
		cash, credit := domain.Milli(r.Cash), domain.Milli(r.Credit)
		out = append(out, domain.TransactionView{ID: r.ID.String(), Kind: r.Kind, Description: r.Description, Period: r.Period,
			Amount: (cash + credit).Units(), Cash: cash.Units(), Credit: credit.Units(), CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// ── Recharges ───────────────────────────────────────────────────────────

func (s *ledgerStore) CreateTopup(ctx context.Context, t *domain.Topup) error {
	return s.db.WithContext(ctx).Create(t).Error
}

func (s *ledgerStore) SetTopupPayment(ctx context.Context, id uuid.UUID, paymentID, environment string) error {
	return s.db.WithContext(ctx).Model(&domain.Topup{}).Where("id = ?", id).
		Updates(map[string]any{"payment_id": paymentID, "environment": environment, "updated_at": time.Now()}).Error
}

func (s *ledgerStore) Topup(ctx context.Context, org, id uuid.UUID) (*domain.Topup, error) {
	var t domain.Topup
	err := s.db.WithContext(ctx).Where("organization_id = ? AND id = ?", org, id).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrTopupNotFound
	}
	return &t, err
}

func (s *ledgerStore) TopupByPayment(ctx context.Context, paymentID string) (*domain.Topup, error) {
	var t domain.Topup
	err := s.db.WithContext(ctx).Where("payment_id = ?", paymentID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrTopupNotFound
	}
	return &t, err
}

func (s *ledgerStore) RecentTopups(ctx context.Context, org uuid.UUID, limit int) ([]domain.Topup, error) {
	out := []domain.Topup{}
	err := s.db.WithContext(ctx).Where("organization_id = ?", org).Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *ledgerStore) HasPendingTopup(ctx context.Context, org uuid.UUID, since time.Time) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.Topup{}).
		Where("organization_id = ? AND status = ? AND created_at > ?", org, domain.TopupPending, since).Count(&n).Error
	return n > 0, err
}

func (s *ledgerStore) PendingTopups(ctx context.Context, olderThan time.Time, limit int) ([]domain.Topup, error) {
	var out []domain.Topup
	err := s.db.WithContext(ctx).Where("status = ? AND created_at < ?", domain.TopupPending, olderThan).
		Order("created_at").Limit(limit).Find(&out).Error
	return out, err
}

func (s *ledgerStore) SettleTopup(ctx context.Context, id uuid.UUID, status, failure string, now time.Time) (*domain.Topup, error) {
	var out domain.Topup
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var t domain.Topup
		if err := tx.First(&t, "id = ?", id).Error; err != nil {
			return err
		}
		if err := lockOrg(tx, t.OrganizationID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&t, "id = ?", id).Error; err != nil {
			return err
		}
		// Une recharge réussie ne change plus ; une recharge expirée ou
		// échouée chez nous peut encore réussir chez XYKPay (l'argent est
		// parti) : elle est alors créditée.
		if t.Status == domain.TopupSucceeded || (t.Status == status && status != domain.TopupSucceeded) {
			out = t
			return nil
		}
		updates := map[string]any{"status": status, "failure_code": failure, "updated_at": now}
		if status == domain.TopupSucceeded {
			ledgerTx, err := domain.NewTransaction(t.OrganizationID, domain.TxTopup, domain.TopupReference(t.ID), t.Currency, "",
				t.CreatedBy, map[string]domain.Milli{domain.CashAccount(t.OrganizationID): t.Amount, domain.AccountXYKPay: -t.Amount}, now)
			if err != nil {
				return err
			}
			if _, err := post(tx, ledgerTx); err != nil {
				return err
			}
			updates["transaction_id"], updates["settled_at"], updates["failure_code"] = ledgerTx.ID, now, ""
		}
		if err := tx.Model(&domain.Topup{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return tx.First(&out, "id = ?", id).Error
	})
	return &out, err
}
