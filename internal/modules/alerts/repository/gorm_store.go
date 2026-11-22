package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/service"
)

// maxRecipients : au-delà, les autres ne sont pas prévenus.
const maxRecipients = 20

type gormStore struct {
	db *gorm.DB
}

// NewStore : stockage en base.
func NewStore(db *gorm.DB) service.Store {
	return &gormStore{db: db}
}

func (s *gormStore) Claim(ctx context.Context, subject uuid.UUID, kind string, every time.Duration) (bool, error) {
	var claimed []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`
		INSERT INTO alert_deliveries (subject_id, kind, sent_at) VALUES (?, ?, now())
		ON CONFLICT (subject_id, kind) DO UPDATE SET sent_at = now()
		WHERE alert_deliveries.sent_at < now() - make_interval(secs => ?)
		RETURNING subject_id`, subject, kind, every.Seconds()).Scan(&claimed).Error
	return len(claimed) > 0, err
}

func (s *gormStore) Recipients(ctx context.Context, project uuid.UUID) ([]domain.Recipient, error) {
	// Ceux qui peuvent réparer : propriétaires et éditeurs. Les liaisons
	// conditionnelles (expiration) sont filtrées ici, une à une.
	var rows []struct {
		ID        uuid.UUID
		Email     string
		FullName  string
		Locale    string
		Condition []byte
	}
	user := iam.KindUser + iam.PrincipalSeparator
	group := iam.KindGroup + iam.PrincipalSeparator
	err := s.db.WithContext(ctx).Raw(`
		WITH b AS (
			SELECT principal, condition FROM iam_bindings
			WHERE resource_name = ? AND role IN (?, ?)
		), p AS (
			SELECT principal, condition FROM b WHERE principal LIKE ? || '%'
			UNION ALL
			SELECT m.principal, b.condition FROM b
			JOIN iam_group_members m ON b.principal = ? || m.group_id::text
		)
		SELECT u.id, u.email, u.full_name, u.locale, p.condition FROM p
		JOIN users u ON p.principal = ? || u.id::text
		WHERE u.deleted_at IS NULL AND u.is_active
		ORDER BY u.email`,
		string(iam.ProjectName(project)), iam.RoleOwner, iam.RoleEditor, user, group, user).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	now := time.Now()
	seen := map[string]bool{}
	out := []domain.Recipient{}
	for _, r := range rows {
		if seen[r.Email] || len(out) == maxRecipients {
			continue
		}
		var cond *iam.Condition
		if len(r.Condition) > 0 && string(r.Condition) != "null" {
			cond = &iam.Condition{}
			if json.Unmarshal(r.Condition, cond) != nil {
				continue // illisible : échec fermé, comme l'évaluateur
			}
		}
		if !iam.ConditionHolds(cond, now) {
			continue
		}
		seen[r.Email] = true
		out = append(out, domain.Recipient{ID: r.ID, Email: r.Email, FullName: r.FullName, Locale: r.Locale})
	}
	return out, nil
}

func (s *gormStore) ProjectName(ctx context.Context, project uuid.UUID) (string, error) {
	var name string
	err := s.db.WithContext(ctx).Raw(`SELECT name FROM projects WHERE id = ?`, project).Scan(&name).Error
	return name, err
}

func (s *gormStore) OrgRecipients(ctx context.Context, org uuid.UUID) ([]domain.Recipient, error) {
	var out []domain.Recipient
	err := s.db.WithContext(ctx).Raw(`
		SELECT u.id, u.email, u.full_name, u.locale FROM iam_bindings b
		JOIN users u ON b.principal = ? || u.id::text
		WHERE b.resource_name = ? AND b.role = ? AND u.deleted_at IS NULL AND u.is_active
		ORDER BY u.email LIMIT ?`,
		iam.KindUser+iam.PrincipalSeparator, string(iam.OrganizationName(org)), iam.RoleOwner, maxRecipients).
		Scan(&out).Error
	return out, err
}

func (s *gormStore) OrgName(ctx context.Context, org uuid.UUID) (string, error) {
	var name string
	err := s.db.WithContext(ctx).Raw(`SELECT display_name FROM organizations WHERE id = ?`, org).Scan(&name).Error
	return name, err
}

func (s *gormStore) Muted(ctx context.Context, category string, users []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(users) == 0 {
		return out, nil
	}
	var ids []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`SELECT user_id FROM alert_mutes WHERE category = ? AND user_id IN ?`, category, users).
		Scan(&ids).Error
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}
