// Package repository : tickets de support dans Postgres (migration 000065).
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
	"github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// GormStore : service.Store.
type GormStore struct {
	DB *gorm.DB
}

var _ service.Store = (*GormStore)(nil)

type ticketRow struct {
	ID               uuid.UUID
	OrganizationID   uuid.UUID
	ProjectID        *uuid.UUID
	ResourceKind     string
	ResourceID       *uuid.UUID
	CreatedBy        string
	CreatedByUserID  *uuid.UUID
	CreatedByEmail   string
	Subject          string
	Description      string
	Status           string
	Priority         string
	Category         string
	Tags             pq.StringArray `gorm:"type:text[]"`
	AssignedToUserID *uuid.UUID
	LastActivityAt   time.Time
	ClosedAt         *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (ticketRow) TableName() string { return "support_tickets" }

func toRow(t *domain.Ticket) ticketRow {
	tags := t.Tags
	if tags == nil {
		tags = []string{}
	}
	return ticketRow{ID: t.ID, OrganizationID: t.OrganizationID, ProjectID: t.ProjectID, ResourceKind: t.ResourceKind, ResourceID: t.ResourceID,
		CreatedBy: t.CreatedBy, CreatedByUserID: t.CreatedByUserID, CreatedByEmail: t.CreatedByEmail, Subject: t.Subject,
		Description: t.Description, Status: string(t.Status), Priority: string(t.Priority), Category: string(t.Category),
		Tags: pq.StringArray(tags), AssignedToUserID: t.AssignedToUserID, LastActivityAt: t.LastActivityAt, ClosedAt: t.ClosedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func (r ticketRow) ticket() domain.Ticket {
	tags := []string(r.Tags)
	if tags == nil {
		tags = []string{}
	}
	return domain.Ticket{ID: r.ID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, ResourceKind: r.ResourceKind, ResourceID: r.ResourceID,
		CreatedBy: r.CreatedBy, CreatedByUserID: r.CreatedByUserID, CreatedByEmail: r.CreatedByEmail, Subject: r.Subject,
		Description: r.Description, Status: domain.Status(r.Status), Priority: domain.Priority(r.Priority), Category: domain.Category(r.Category),
		Tags: tags, AssignedToUserID: r.AssignedToUserID, LastActivityAt: r.LastActivityAt, ClosedAt: r.ClosedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

type messageRow struct {
	ID             uuid.UUID
	TicketID       uuid.UUID
	OrganizationID uuid.UUID
	AuthorUserID   *uuid.UUID
	AuthorRole     string
	Body           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (messageRow) TableName() string { return "support_messages" }

type eventRow struct {
	ID             uuid.UUID
	TicketID       uuid.UUID
	OrganizationID uuid.UUID
	Type           string
	ActorUserID    *uuid.UUID
	Payload        []byte `gorm:"type:jsonb"`
	CreatedAt      time.Time
}

func (eventRow) TableName() string { return "support_ticket_events" }

type attachmentRow struct {
	ID        uuid.UUID
	TicketID  uuid.UUID
	MessageID *uuid.UUID
	FileName  string
	MimeType  string
	Size      int64
	ObjectKey string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (attachmentRow) TableName() string { return "support_attachments" }

type macroRow struct {
	ID        uuid.UUID
	Title     string
	Body      string
	Category  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (macroRow) TableName() string { return "support_macros" }

func eventRows(events []domain.Event) ([]eventRow, error) {
	out := make([]eventRow, 0, len(events))
	for _, e := range events {
		payload := []byte("{}")
		if len(e.Payload) > 0 {
			b, err := json.Marshal(e.Payload)
			if err != nil {
				return nil, err
			}
			payload = b
		}
		out = append(out, eventRow{ID: e.ID, TicketID: e.TicketID, OrganizationID: e.OrganizationID, Type: string(e.Type),
			ActorUserID: e.ActorUserID, Payload: payload, CreatedAt: e.CreatedAt})
	}
	return out, nil
}

func (s *GormStore) CreateTicket(ctx context.Context, t *domain.Ticket, ev domain.Event) error {
	evs, err := eventRows([]domain.Event{ev})
	if err != nil {
		return err
	}
	row := toRow(t)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Create(&evs).Error
	})
}

func (s *GormStore) SaveTicket(ctx context.Context, t *domain.Ticket, msg *domain.Message, events []domain.Event) error {
	evs, err := eventRows(events)
	if err != nil {
		return err
	}
	row := toRow(t)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&ticketRow{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": row.Status, "priority": row.Priority, "category": row.Category, "tags": row.Tags,
			"assigned_to_user_id": row.AssignedToUserID, "last_activity_at": row.LastActivityAt,
			"closed_at": row.ClosedAt, "updated_at": row.UpdatedAt,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return domain.ErrTicketNotFound
		}
		if msg != nil {
			m := messageRow{ID: msg.ID, TicketID: msg.TicketID, OrganizationID: msg.OrganizationID, AuthorUserID: msg.AuthorUserID,
				AuthorRole: string(msg.AuthorRole), Body: msg.Body, CreatedAt: msg.CreatedAt, UpdatedAt: msg.UpdatedAt}
			if err := tx.Create(&m).Error; err != nil {
				return err
			}
		}
		if len(evs) == 0 {
			return nil
		}
		return tx.Create(&evs).Error
	})
}

func (s *GormStore) Ticket(ctx context.Context, id uuid.UUID) (*domain.Ticket, error) {
	var row ticketRow
	err := s.DB.WithContext(ctx).Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	t := row.ticket()
	return &t, nil
}

// likeEscape : % et _ cherchés tels quels.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *GormStore) ListTickets(ctx context.Context, f domain.Filter) ([]domain.Ticket, int64, error) {
	q := s.DB.WithContext(ctx).Model(&ticketRow{})
	if f.OrganizationID != nil {
		q = q.Where("organization_id = ?", *f.OrganizationID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if f.Category != "" {
		q = q.Where("category = ?", string(f.Category))
	}
	if f.Priority != "" {
		q = q.Where("priority = ?", string(f.Priority))
	}
	if f.Search != "" {
		like := "%" + likeEscape(f.Search) + "%"
		q = q.Where("(subject ILIKE ? OR description ILIKE ? OR created_by_email ILIKE ?)", like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if f.StaffOrder {
		q = q.Order("CASE WHEN status IN ('OPEN','IN_PROGRESS') THEN 0 WHEN status = 'WAITING_CUSTOMER' THEN 1 ELSE 2 END").
			Order("CASE priority WHEN 'URGENT' THEN 0 WHEN 'HIGH' THEN 1 WHEN 'MEDIUM' THEN 2 ELSE 3 END").
			Order("last_activity_at ASC")
	} else {
		q = q.Order("last_activity_at DESC")
	}
	var rows []ticketRow
	if err := q.Order("id").Limit(f.Limit).Offset(f.Offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]domain.Ticket, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ticket())
	}
	return out, total, nil
}

func (s *GormStore) Messages(ctx context.Context, ticket uuid.UUID) ([]domain.Message, error) {
	var rows []messageRow
	if err := s.DB.WithContext(ctx).Where("ticket_id = ?", ticket).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Message{ID: r.ID, TicketID: r.TicketID, OrganizationID: r.OrganizationID, AuthorUserID: r.AuthorUserID,
			AuthorRole: domain.AuthorRole(r.AuthorRole), Body: r.Body, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

func (s *GormStore) Events(ctx context.Context, ticket uuid.UUID) ([]domain.Event, error) {
	var rows []eventRow
	if err := s.DB.WithContext(ctx).Where("ticket_id = ?", ticket).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Event, 0, len(rows))
	for _, r := range rows {
		e := domain.Event{ID: r.ID, TicketID: r.TicketID, OrganizationID: r.OrganizationID, Type: domain.EventType(r.Type),
			ActorUserID: r.ActorUserID, CreatedAt: r.CreatedAt}
		if len(r.Payload) > 0 {
			_ = json.Unmarshal(r.Payload, &e.Payload)
		}
		if len(e.Payload) == 0 {
			e.Payload = nil
		}
		out = append(out, e)
	}
	return out, nil
}

func attachmentOf(r attachmentRow) domain.Attachment {
	return domain.Attachment{ID: r.ID, TicketID: r.TicketID, MessageID: r.MessageID, FileName: r.FileName, MimeType: r.MimeType,
		Size: r.Size, ObjectKey: r.ObjectKey, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (s *GormStore) Attachments(ctx context.Context, ticket uuid.UUID) ([]domain.Attachment, error) {
	var rows []attachmentRow
	if err := s.DB.WithContext(ctx).Where("ticket_id = ?", ticket).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Attachment, 0, len(rows))
	for _, r := range rows {
		out = append(out, attachmentOf(r))
	}
	return out, nil
}

func (s *GormStore) AddAttachment(ctx context.Context, a *domain.Attachment) error {
	row := attachmentRow{ID: a.ID, TicketID: a.TicketID, MessageID: a.MessageID, FileName: a.FileName, MimeType: a.MimeType,
		Size: a.Size, ObjectKey: a.ObjectKey, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	return s.DB.WithContext(ctx).Create(&row).Error
}

func (s *GormStore) Attachment(ctx context.Context, ticket, id uuid.UUID) (*domain.Attachment, error) {
	var row attachmentRow
	err := s.DB.WithContext(ctx).Where("id = ? AND ticket_id = ?", id, ticket).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAttachmentNotFound
	}
	if err != nil {
		return nil, err
	}
	a := attachmentOf(row)
	return &a, nil
}

func (s *GormStore) MessageOnTicket(ctx context.Context, ticket, msg uuid.UUID) (bool, error) {
	var n int64
	err := s.DB.WithContext(ctx).Model(&messageRow{}).Where("id = ? AND ticket_id = ?", msg, ticket).Count(&n).Error
	return n > 0, err
}

func (s *GormStore) ProjectInOrganization(ctx context.Context, project, org uuid.UUID) (bool, error) {
	var n int64
	err := s.DB.WithContext(ctx).Table("projects").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", project, org).Count(&n).Error
	return n > 0, err
}

func (s *GormStore) Contacts(ctx context.Context, users []uuid.UUID) ([]service.Contact, error) {
	var out []service.Contact
	if len(users) == 0 {
		return out, nil
	}
	err := s.DB.WithContext(ctx).Raw(`SELECT id, email, full_name, locale FROM users
		WHERE id IN ? AND deleted_at IS NULL AND is_active AND email <> '' ORDER BY email`, users).Scan(&out).Error
	return out, err
}

func (s *GormStore) MemberContacts(ctx context.Context, org uuid.UUID, users []uuid.UUID) ([]service.Contact, error) {
	var out []service.Contact
	if len(users) == 0 {
		return out, nil
	}
	orgName := "organizations/" + org.String()
	err := s.DB.WithContext(ctx).Raw(`SELECT u.id, u.email, u.full_name, u.locale FROM users u
		WHERE u.id IN @users AND u.deleted_at IS NULL AND u.is_active AND u.email <> ''
		AND EXISTS (
			SELECT 1 FROM iam_bindings b
			WHERE (b.resource_name = @org OR b.resource_name IN (SELECT 'projects/' || p.id::text FROM projects p WHERE p.organization_id = @orgID))
			AND (b.principal = 'user:' || u.id::text OR b.principal IN (
				SELECT 'group:' || g.id::text FROM iam_groups g JOIN iam_group_members gm ON gm.group_id = g.id
				WHERE gm.principal = 'user:' || u.id::text)))
		ORDER BY u.email`, map[string]any{"users": users, "org": orgName, "orgID": org}).Scan(&out).Error
	return out, err
}

func (s *GormStore) UserExists(ctx context.Context, user uuid.UUID) (bool, error) {
	var n int64
	err := s.DB.WithContext(ctx).Table("users").Where("id = ? AND deleted_at IS NULL", user).Count(&n).Error
	return n > 0, err
}

func (s *GormStore) Summary(ctx context.Context) (domain.Summary, error) {
	var out domain.Summary
	err := s.DB.WithContext(ctx).Raw(`SELECT
		COUNT(*) FILTER (WHERE status = 'OPEN') AS open,
		COUNT(*) FILTER (WHERE status = 'IN_PROGRESS') AS in_progress,
		COUNT(*) FILTER (WHERE status = 'WAITING_CUSTOMER') AS waiting_customer,
		COUNT(*) FILTER (WHERE status IN ('OPEN','IN_PROGRESS') AND priority IN ('HIGH','URGENT')) AS high_priority
		FROM support_tickets WHERE status NOT IN ('RESOLVED','CLOSED')`).Scan(&out).Error
	return out, err
}

func macroOf(r macroRow) domain.Macro {
	return domain.Macro{ID: r.ID, Title: r.Title, Body: r.Body, Category: domain.Category(r.Category), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (s *GormStore) Macros(ctx context.Context) ([]domain.Macro, error) {
	var rows []macroRow
	if err := s.DB.WithContext(ctx).Order("title, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Macro, 0, len(rows))
	for _, r := range rows {
		out = append(out, macroOf(r))
	}
	return out, nil
}

func (s *GormStore) CreateMacro(ctx context.Context, m *domain.Macro) error {
	row := macroRow{ID: m.ID, Title: m.Title, Body: m.Body, Category: string(m.Category), CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
	return s.DB.WithContext(ctx).Create(&row).Error
}

func (s *GormStore) UpdateMacro(ctx context.Context, m *domain.Macro) error {
	res := s.DB.WithContext(ctx).Model(&macroRow{}).Where("id = ?", m.ID).
		Updates(map[string]any{"title": m.Title, "body": m.Body, "category": string(m.Category), "updated_at": m.UpdatedAt})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrMacroNotFound
	}
	var row macroRow
	if err := s.DB.WithContext(ctx).Where("id = ?", m.ID).Take(&row).Error; err != nil {
		return err
	}
	*m = macroOf(row)
	return nil
}

func (s *GormStore) DeleteMacro(ctx context.Context, id uuid.UUID) error {
	res := s.DB.WithContext(ctx).Where("id = ?", id).Delete(&macroRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrMacroNotFound
	}
	return nil
}
