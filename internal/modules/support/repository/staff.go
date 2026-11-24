package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
	"github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// Lectures du back-office (service.StaffStore).

var _ service.StaffStore = (*GormStore)(nil)

// agentsQuery : comptes ayant un rôle sur "platform", directement ou par un
// groupe ; comptes supprimés ou désactivés exclus.
const agentsQuery = `SELECT u.id, u.email, u.full_name FROM users u
	WHERE u.deleted_at IS NULL AND u.is_active
	AND ('user:' || u.id::text) IN (
		SELECT b.principal FROM iam_bindings b WHERE b.resource_name = ?
		UNION
		SELECT m.principal FROM iam_bindings b
		JOIN iam_group_members m ON b.principal = 'group:' || m.group_id::text
		WHERE b.resource_name = ?)`

type agentRow struct {
	ID       uuid.UUID
	Email    string
	FullName string
}

func (r agentRow) agent() service.Agent {
	return service.Agent{UserID: r.ID, Email: r.Email, FullName: r.FullName}
}

func (s *GormStore) Agents(ctx context.Context) ([]service.Agent, error) {
	var rows []agentRow
	platform := iam.Platform.String()
	if err := s.DB.WithContext(ctx).Raw(agentsQuery+` ORDER BY u.full_name, u.email`, platform, platform).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]service.Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.agent())
	}
	return out, nil
}

func (s *GormStore) IsAgent(ctx context.Context, user uuid.UUID) (bool, error) {
	var n int64
	platform := iam.Platform.String()
	err := s.DB.WithContext(ctx).Raw(`SELECT count(*) FROM (`+agentsQuery+` AND u.id = ?) a`, platform, platform, user).
		Scan(&n).Error
	return n > 0, err
}

type factsRow struct {
	ID                    uuid.UUID
	OrganizationName      string
	AgentID               *uuid.UUID
	AgentEmail            *string
	AgentName             *string
	LastCustomerMessageAt *time.Time
	FirstResponseAt       *time.Time
}

// TicketFacts : une requête pour toute la page. Le dernier message du client
// retombe sur la création du ticket ; la première réponse ignore les notes
// internes.
func (s *GormStore) TicketFacts(ctx context.Context, tickets []domain.Ticket) (map[uuid.UUID]service.TicketFacts, error) {
	out := make(map[uuid.UUID]service.TicketFacts, len(tickets))
	if len(tickets) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(tickets))
	for _, t := range tickets {
		ids = append(ids, t.ID)
	}
	var rows []factsRow
	err := s.DB.WithContext(ctx).Raw(`SELECT t.id,
			COALESCE(o.display_name, '') AS organization_name,
			u.id AS agent_id, u.email AS agent_email, u.full_name AS agent_name,
			COALESCE((SELECT MAX(m.created_at) FROM support_messages m
				WHERE m.ticket_id = t.id AND m.author_role = ?), t.created_at) AS last_customer_message_at,
			(SELECT MIN(m.created_at) FROM support_messages m
				WHERE m.ticket_id = t.id AND m.author_role = ? AND NOT m.internal) AS first_response_at
		FROM support_tickets t
		LEFT JOIN organizations o ON o.id = t.organization_id
		LEFT JOIN users u ON u.id = t.assigned_to_user_id
		WHERE t.id IN ?`, string(domain.AuthorCustomer), string(domain.AuthorSupportAgent), ids).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		f := service.TicketFacts{OrganizationName: r.OrganizationName,
			LastCustomerMessageAt: r.LastCustomerMessageAt, FirstResponseAt: r.FirstResponseAt}
		if r.AgentID != nil {
			a := service.Agent{UserID: *r.AgentID}
			if r.AgentEmail != nil {
				a.Email = *r.AgentEmail
			}
			if r.AgentName != nil {
				a.FullName = *r.AgentName
			}
			f.AssignedTo = &a
		}
		out[r.ID] = f
	}
	return out, nil
}

type orgRow struct {
	ID          uuid.UUID
	DisplayName string
	Kind        string
	CreatedAt   time.Time
	OwnerID     *uuid.UUID
	OwnerEmail  *string
	OwnerName   *string
}

type projectRow struct {
	ID        uuid.UUID
	Name      string
	Workloads int64
	Sites     int64
	VPS       int64 `gorm:"column:vps"`
	Databases int64
}

// Organization : propriétaire = créateur d'une organisation personnelle, à
// défaut le plus ancien roles/owner de l'organisation.
func (s *GormStore) Organization(ctx context.Context, org uuid.UUID) (*service.OrganizationContext, []service.ProjectContext, error) {
	db := s.DB.WithContext(ctx)
	var o orgRow
	res := db.Raw(`SELECT o.id, o.display_name, o.kind, o.created_at,
			u.id AS owner_id, u.email AS owner_email, u.full_name AS owner_name
		FROM organizations o
		LEFT JOIN users u ON u.deleted_at IS NULL AND u.id = COALESCE(o.owner_user_id, (
			SELECT substring(b.principal FROM 6)::uuid FROM iam_bindings b
			WHERE b.resource_name = 'organizations/' || o.id::text AND b.role = 'roles/owner'
				AND b.principal ~ '^user:[0-9a-f-]{36}$'
			ORDER BY b.created_at LIMIT 1))
		WHERE o.id = ? AND o.deleted_at IS NULL`, org).Scan(&o)
	if res.Error != nil {
		return nil, nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil, errors.New("support: ticket organization not found")
	}
	out := &service.OrganizationContext{ID: o.ID, Name: o.DisplayName, Kind: o.Kind, CreatedAt: o.CreatedAt}
	if o.OwnerID != nil {
		p := &service.Person{UserID: *o.OwnerID}
		if o.OwnerEmail != nil {
			p.Email = *o.OwnerEmail
		}
		if o.OwnerName != nil {
			p.FullName = *o.OwnerName
		}
		out.Owner = p
	}
	var rows []projectRow
	if err := db.Raw(`SELECT p.id, p.name,
			(SELECT count(*) FROM workloads w WHERE w.project_id = p.id AND w.status NOT IN ('DELETING', 'DELETED')) AS workloads,
			(SELECT count(*) FROM sites s WHERE s.project_id = p.id) AS sites,
			(SELECT count(*) FROM compute_vps v WHERE v.project_id = p.id AND v.state <> 'deleted') AS vps,
			(SELECT count(*) FROM databases d WHERE d.project_id = p.id) AS databases
		FROM projects p WHERE p.organization_id = ? AND p.deleted_at IS NULL
		ORDER BY p.created_at`, org).Scan(&rows).Error; err != nil {
		return nil, nil, err
	}
	projects := make([]service.ProjectContext, 0, len(rows))
	for _, r := range rows {
		projects = append(projects, service.ProjectContext{ID: r.ID, Name: r.Name,
			Workloads: r.Workloads, Sites: r.Sites, VPS: r.VPS, Databases: r.Databases})
	}
	return out, projects, nil
}

// resourceQueries : genres de ressource connus d'un ticket.
var resourceQueries = map[string]string{
	"vps":      `SELECT id, name, state AS status, project_id FROM compute_vps WHERE id = ?`,
	"workload": `SELECT id, name, status, project_id FROM workloads WHERE id = ?`,
	"site":     `SELECT id, name, '' AS status, project_id FROM sites WHERE id = ?`,
	"database": `SELECT id, name, status, project_id FROM databases WHERE id = ?`,
}

type resourceRow struct {
	ID        uuid.UUID
	Name      string
	Status    string
	ProjectID uuid.UUID
}

func (s *GormStore) Resource(ctx context.Context, kind string, id uuid.UUID) (*service.ResourceContext, error) {
	q, ok := resourceQueries[kind]
	if !ok {
		return nil, nil
	}
	var r resourceRow
	res := s.DB.WithContext(ctx).Raw(q, id).Scan(&r)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	return &service.ResourceContext{Kind: kind, ID: r.ID, Name: r.Name, Status: r.Status, ProjectID: r.ProjectID}, nil
}
