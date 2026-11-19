package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Plans : catalogue des plans et plan de chaque organisation.
type Plans interface {
	Plans(ctx context.Context) ([]domain.Plan, error)
	Plan(ctx context.Context, id string) (*domain.Plan, error)
	SavePlan(ctx context.Context, p domain.Plan, by string) error
	// OrgPlan : plan de l'organisation (défaut sans compte) et depuis quand.
	OrgPlan(ctx context.Context, org uuid.UUID) (string, *time.Time, error)
	SetOrgPlan(ctx context.Context, org uuid.UUID, plan string, at time.Time) error
	// SetOverride / DeleteOverride : exception de l'équipe aux limites du plan
	// pour un projet.
	SetOverride(ctx context.Context, project uuid.UUID, cpu, memory, storage, by string, at time.Time) error
	DeleteOverride(ctx context.Context, project uuid.UUID) (bool, error)
	// Overrides : projets de l'organisation soustraits au plan par une exception.
	Overrides(ctx context.Context, org uuid.UUID) ([]uuid.UUID, error)
}

type planRow struct {
	ID           string
	Name         string
	Position     int
	Public       bool
	CPULimit     string `gorm:"column:cpu_limit"`
	MemoryLimit  string
	StorageLimit string
	Fees         []byte
}

func (r planRow) plan() (domain.Plan, error) {
	p := domain.Plan{ID: r.ID, Name: r.Name, Position: r.Position, Public: r.Public, CPULimit: r.CPULimit,
		MemoryLimit: r.MemoryLimit, StorageLimit: r.StorageLimit, Fees: map[string]int64{}}
	if len(r.Fees) > 0 {
		if err := json.Unmarshal(r.Fees, &p.Fees); err != nil {
			return domain.Plan{}, err
		}
	}
	return p, nil
}

type plansStore struct{ db *gorm.DB }

// NewPlans : plans en base.
func NewPlans(db *gorm.DB) Plans { return &plansStore{db: db} }

const planCols = `id, name, position, public, cpu_limit, memory_limit, storage_limit, fees`

func (s *plansStore) Plans(ctx context.Context) ([]domain.Plan, error) {
	var rows []planRow
	if err := s.db.WithContext(ctx).Raw(`SELECT ` + planCols + ` FROM billing_plans ORDER BY position, id`).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Plan, 0, len(rows))
	for _, r := range rows {
		p, err := r.plan()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *plansStore) Plan(ctx context.Context, id string) (*domain.Plan, error) {
	var rows []planRow
	if err := s.db.WithContext(ctx).Raw(`SELECT `+planCols+` FROM billing_plans WHERE id = ?`, id).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, domain.ErrPlanNotFound
	}
	p, err := rows[0].plan()
	return &p, err
}

func (s *plansStore) SavePlan(ctx context.Context, p domain.Plan, by string) error {
	fees, err := json.Marshal(p.Fees)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO billing_plans (id, name, position, public, cpu_limit, memory_limit, storage_limit, fees, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, position = excluded.position, public = excluded.public,
			cpu_limit = excluded.cpu_limit, memory_limit = excluded.memory_limit, storage_limit = excluded.storage_limit,
			fees = excluded.fees, updated_by = excluded.updated_by, updated_at = NOW()`,
		p.ID, p.Name, p.Position, p.Public, p.CPULimit, p.MemoryLimit, p.StorageLimit, string(fees), by).Error
}

func (s *plansStore) OrgPlan(ctx context.Context, org uuid.UUID) (string, *time.Time, error) {
	var rows []struct {
		PlanID    string
		PlanSince *time.Time
	}
	err := s.db.WithContext(ctx).Raw(`SELECT plan_id, plan_since FROM billing_accounts WHERE organization_id = ?`, org).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return domain.DefaultPlan, nil, err
	}
	return rows[0].PlanID, rows[0].PlanSince, nil
}

func (s *plansStore) SetOrgPlan(ctx context.Context, org uuid.UUID, plan string, at time.Time) error {
	err := s.db.WithContext(ctx).Exec(`INSERT INTO billing_accounts (organization_id, plan_id, plan_since) VALUES (?, ?, ?)
		ON CONFLICT (organization_id) DO UPDATE SET plan_id = excluded.plan_id, plan_since = excluded.plan_since, updated_at = NOW()`,
		org, plan, at).Error
	if err != nil && errors.Is(err, gorm.ErrForeignKeyViolated) {
		return domain.ErrPlanNotFound
	}
	return err
}

func (s *plansStore) SetOverride(ctx context.Context, project uuid.UUID, cpu, memory, storage, by string, at time.Time) error {
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO billing_limit_overrides (project_id, cpu_limit, memory_limit, storage_limit, set_by, set_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET cpu_limit = excluded.cpu_limit, memory_limit = excluded.memory_limit,
			storage_limit = excluded.storage_limit, set_by = excluded.set_by, set_at = excluded.set_at`,
		project, cpu, memory, storage, by, at).Error
}

func (s *plansStore) DeleteOverride(ctx context.Context, project uuid.UUID) (bool, error) {
	res := s.db.WithContext(ctx).Exec(`DELETE FROM billing_limit_overrides WHERE project_id = ?`, project)
	return res.RowsAffected > 0, res.Error
}

func (s *plansStore) Overrides(ctx context.Context, org uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithContext(ctx).Raw(`SELECT o.project_id FROM billing_limit_overrides o
		JOIN projects p ON p.id = o.project_id WHERE p.organization_id = ?`, org).Scan(&ids).Error
	return ids, err
}
