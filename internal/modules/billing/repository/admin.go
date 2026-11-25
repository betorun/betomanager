package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Admin : lectures de l'équipe de la plateforme (organisations, recettes).
// Lecture seule des tables de l'IAM et des projets.
type Admin interface {
	SearchOrgs(ctx context.Context, query string, limit int) ([]OrgRow, error)
	Org(ctx context.Context, id uuid.UUID) (*OrgRow, error)
	OrgProjects(ctx context.Context, id uuid.UUID) ([]domain.OrgProject, error)
	Revenue(ctx context.Context, from time.Time) ([]domain.RevenueDay, error)
	TopupCounts(ctx context.Context, from time.Time) (pending, failed int, err error)
}

// OrgRow : organisation et son compte.
type OrgRow struct {
	ID       uuid.UUID
	Name     string
	Kind     string
	Owner    string
	Country  string
	Projects int
	// MaxProjects : plafond de projets de l'organisation.
	MaxProjects int
	Currency    string
	Charging    bool
	Balance     int64 // millièmes : argent + crédits
	CreatedAt   time.Time
}

type adminStore struct{ db *gorm.DB }

// NewAdmin : lectures de l'administration.
func NewAdmin(db *gorm.DB) Admin { return &adminStore{db: db} }

// orgSelect : propriétaire = celui d'une organisation personnelle, sinon le
// premier owner de la policy de l'organisation.
const orgSelect = `
	SELECT o.id, o.display_name AS name, o.kind, o.country,
		COALESCE(pu.email, (
			SELECT u.email FROM iam_bindings b JOIN users u ON b.principal = @user || u.id::text
			WHERE b.resource_name = 'organizations/' || o.id::text AND b.role = @owner ORDER BY u.email LIMIT 1), '') AS owner,
		(SELECT COUNT(*) FROM projects p WHERE p.organization_id = o.id AND p.deleted_at IS NULL) AS projects,
		o.max_projects,
		COALESCE(a.currency, @currency) AS currency, COALESCE(a.charging, FALSE) AS charging,
		(SELECT COALESCE(SUM(e.amount), 0) FROM billing_ledger_entries e
			WHERE e.account IN ('customer:' || o.id::text || ':cash', 'customer:' || o.id::text || ':credit')) AS balance,
		o.created_at
	FROM organizations o
	LEFT JOIN users pu ON pu.id = o.owner_user_id AND o.kind = 'PERSONAL'
	LEFT JOIN billing_accounts a ON a.organization_id = o.id
	WHERE o.deleted_at IS NULL`

func (s *adminStore) args() map[string]any {
	return map[string]any{"user": iam.KindUser + iam.PrincipalSeparator, "owner": iam.RoleOwner, "currency": domain.DefaultCurrency}
}

func (s *adminStore) SearchOrgs(ctx context.Context, query string, limit int) ([]OrgRow, error) {
	args := s.args()
	q := orgSelect
	if query = strings.TrimSpace(query); query != "" {
		args["q"] = "%" + strings.ToLower(query) + "%"
		q += ` AND (LOWER(o.display_name) LIKE @q OR LOWER(COALESCE(pu.email, '')) LIKE @q
			OR EXISTS (SELECT 1 FROM projects p WHERE p.organization_id = o.id AND p.deleted_at IS NULL
				AND (LOWER(p.name) LIKE @q OR p.id::text = @exact))
			OR o.id::text = @exact)`
		args["exact"] = strings.ToLower(query)
	}
	args["limit"] = limit
	q += ` ORDER BY o.created_at DESC LIMIT @limit`
	var rows []OrgRow
	err := s.db.WithContext(ctx).Raw(q, args).Scan(&rows).Error
	return rows, err
}

func (s *adminStore) Org(ctx context.Context, id uuid.UUID) (*OrgRow, error) {
	args := s.args()
	args["id"] = id
	var rows []OrgRow
	if err := s.db.WithContext(ctx).Raw(orgSelect+` AND o.id = @id`, args).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, domain.ErrOrganizationNotFound
	}
	return &rows[0], nil
}

func (s *adminStore) OrgProjects(ctx context.Context, id uuid.UUID) ([]domain.OrgProject, error) {
	var rows []struct {
		ID     uuid.UUID
		Name   string
		Status string
	}
	err := s.db.WithContext(ctx).Raw(`SELECT id, name, status FROM projects WHERE organization_id = ? AND deleted_at IS NULL ORDER BY name`, id).
		Scan(&rows).Error
	out := make([]domain.OrgProject, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.OrgProject{ID: r.ID.String(), Name: r.Name, Status: r.Status})
	}
	return out, err
}

// Revenue : par jour (UTC) et par monnaie : argent reçu par recharge, usage
// payé en argent, crédits offerts.
func (s *adminStore) Revenue(ctx context.Context, from time.Time) ([]domain.RevenueDay, error) {
	var rows []struct {
		Day      time.Time
		Currency string
		Topups   int64
		Usage    int64
		Credits  int64
	}
	err := s.db.WithContext(ctx).Raw(`
		SELECT date_trunc('day', e.created_at AT TIME ZONE 'UTC') AS day, e.currency,
			COALESCE(-SUM(e.amount) FILTER (WHERE e.account = ?), 0) AS topups,
			COALESCE(SUM(e.amount) FILTER (WHERE e.account = ?), 0) AS usage,
			COALESCE(-SUM(e.amount) FILTER (WHERE e.account = ? AND t.kind = ?), 0) AS credits
		FROM billing_ledger_entries e JOIN billing_transactions t ON t.id = e.transaction_id
		WHERE e.created_at >= ? AND e.account LIKE 'platform:%'
		GROUP BY 1, 2 ORDER BY 1 DESC, 2`,
		domain.AccountXYKPay, domain.AccountRevenue, domain.AccountPromotions, domain.TxCreditGrant, from).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]domain.RevenueDay, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.RevenueDay{Day: r.Day.Format("2006-01-02"), Currency: r.Currency,
			Topups: domain.Milli(r.Topups).Units(), Usage: domain.Milli(r.Usage).Units(), Credits: domain.Milli(r.Credits).Units()})
	}
	return out, nil
}

func (s *adminStore) TopupCounts(ctx context.Context, from time.Time) (int, int, error) {
	var r struct{ Pending, Failed int }
	err := s.db.WithContext(ctx).Raw(`SELECT
			COUNT(*) FILTER (WHERE status = ?) AS pending,
			COUNT(*) FILTER (WHERE status IN (?, ?)) AS failed
		FROM billing_topups WHERE created_at >= ?`, domain.TopupPending, domain.TopupFailed, domain.TopupExpired, from).Scan(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, nil
	}
	return r.Pending, r.Failed, err
}
