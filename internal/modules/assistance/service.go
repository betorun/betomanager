// Package assistance : accès d'assistance de l'équipe betorun chez un client
// (ADR 0017).
//
// Une attribution donne à UNE personne nommée de l'équipe, pour un temps
// borné, le rôle réservé roles/support.assistant sur une organisation. Elle
// est accordée par le client (après une demande liée à un ticket, ou de
// lui-même) ou ouverte en bris de glace (incident, 1 h, propriétaires
// prévenus). La table est la source de vérité ; la liaison IAM en découle et
// porte l'échéance (condition), si bien qu'un accès échu n'accorde plus rien
// même avant le passage du ramasseur.
package assistance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thekrauss/kubemanager/internal/core/apierr"

	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
)

// Modes et statuts.
const (
	ModeConsent    = "consent"
	ModeBreakglass = "breakglass"

	StatusRequested = "requested"
	StatusActive    = "active"
	StatusDenied    = "denied"
	StatusRevoked   = "revoked"
	StatusExpired   = "expired"
	StatusClosed    = "closed"
)

// Bornes (ADR 0017 §3).
const (
	DefaultHours = 4
	MaxHours     = 24
	// BreakglassMax : durée d'un bris de glace.
	BreakglassMax = time.Hour
	// RequestTTL : une demande sans réponse expire.
	RequestTTL = 72 * time.Hour
	minReason  = 20
	maxReason  = 500
)

// Événements envoyés aux propriétaires.
const (
	NoticeRequested  = "requested"
	NoticeGranted    = "granted"
	NoticeBreakglass = "breakglass"
)

// Notifier : e-mails aux propriétaires de l'organisation.
type Notifier interface {
	AssistanceNotice(ctx context.Context, org uuid.UUID, kind, staff, reason string, until *time.Time)
}

// Person : qui agit.
type Person struct {
	ID    uuid.UUID
	Email string
}

func (p Person) principal() string { return string(iam.UserPrincipal(p.ID)) }

// Grant : une attribution.
type Grant struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID uuid.UUID  `gorm:"type:uuid" json:"organization_id"`
	TicketID       *uuid.UUID `gorm:"type:uuid" json:"ticket_id,omitempty"`
	StaffUserID    uuid.UUID  `gorm:"type:uuid" json:"staff_user_id"`
	StaffEmail     string     `json:"staff_email"`
	Mode           string     `json:"mode"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason"`
	RequestedHours int        `json:"requested_hours"`
	RequestedAt    time.Time  `json:"requested_at"`
	DecidedBy      *uuid.UUID `gorm:"type:uuid" json:"-"`
	DecidedEmail   string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	EndedBy        string     `json:"ended_by,omitempty"`
	EndNote        string     `json:"end_note,omitempty"`
}

func (Grant) TableName() string { return "assistance_grants" }

// Erreurs.
var (
	ErrNotFound    = betoerrors.New(betoerrors.CodeNotFound, "assistance access not found")
	ErrAlreadyOpen = apierr.WithReason(betoerrors.New(betoerrors.CodeConflict,
		"an assistance request or access is already open for this person on this organization"), "ASSISTANCE_ALREADY_OPEN")
	ErrNotPending = apierr.WithReason(betoerrors.New(betoerrors.CodeConflict,
		"this assistance access is no longer pending"), "ASSISTANCE_NOT_PENDING")
	ErrNotActive = apierr.WithReason(betoerrors.New(betoerrors.CodeConflict,
		"this assistance access is not active"), "ASSISTANCE_NOT_ACTIVE")
	ErrNoAssignee = apierr.WithReason(betoerrors.New(betoerrors.CodeInvalidInput,
		"the ticket has no assigned agent to grant access to"), "ASSISTANCE_NO_ASSIGNEE")
)

// Service : les attributions.
type Service struct {
	DB     *gorm.DB
	Audit  audit.Writer
	Notify Notifier
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func checkReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < minReason || n > maxReason {
		return "", apierr.WithReason(betoerrors.New(betoerrors.CodeInvalidInput, "a reason of 20 to 500 characters is required"), "ASSISTANCE_REASON_INVALID")
	}
	return reason, nil
}

func checkHours(h int) (int, error) {
	if h == 0 {
		return DefaultHours, nil
	}
	if h < 1 || h > MaxHours {
		return 0, apierr.WithReason(betoerrors.New(betoerrors.CodeInvalidInput, "the duration must be between 1 and 24 hours"), "ASSISTANCE_DURATION_INVALID")
	}
	return h, nil
}

// Request : une personne de l'équipe demande l'accès depuis un ticket.
func (s *Service) Request(ctx context.Context, staff Person, org uuid.UUID, ticket *uuid.UUID, reason string, hours int) (Grant, error) {
	reason, err := checkReason(reason)
	if err != nil {
		return Grant{}, err
	}
	if hours, err = checkHours(hours); err != nil {
		return Grant{}, err
	}
	g := Grant{ID: uuid.New(), OrganizationID: org, TicketID: ticket, StaffUserID: staff.ID, StaffEmail: staff.Email,
		Mode: ModeConsent, Status: StatusRequested, Reason: reason, RequestedHours: hours, RequestedAt: s.now()}
	if err := s.insert(ctx, &g); err != nil {
		return Grant{}, err
	}
	s.journal(ctx, staff.principal(), "organization.assistance.requested", g, nil)
	s.notify(ctx, org, NoticeRequested, staff.Email, reason, nil)
	return g, nil
}

// BreakGlass : accès ouvert sans l'accord du client (incident), 1 h au plus.
func (s *Service) BreakGlass(ctx context.Context, staff Person, org uuid.UUID, ticket *uuid.UUID, reason string, d time.Duration) (Grant, error) {
	reason, err := checkReason(reason)
	if err != nil {
		return Grant{}, err
	}
	if d <= 0 || d > BreakglassMax {
		d = BreakglassMax
	}
	now := s.now()
	exp := now.Add(d)
	g := Grant{ID: uuid.New(), OrganizationID: org, TicketID: ticket, StaffUserID: staff.ID, StaffEmail: staff.Email,
		Mode: ModeBreakglass, Status: StatusActive, Reason: reason, RequestedHours: 1, RequestedAt: now,
		DecidedBy: &staff.ID, DecidedEmail: staff.Email, DecidedAt: &now, ExpiresAt: &exp}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := insertTx(tx, &g); err != nil {
			return err
		}
		return bind(ctx, tx, g, staff.principal())
	})
	if err != nil {
		return Grant{}, err
	}
	s.journal(ctx, staff.principal(), "organization.assistance.breakglass", g, nil)
	s.notify(ctx, org, NoticeBreakglass, staff.Email, reason, &exp)
	return g, nil
}

// Approve : un propriétaire accepte une demande (durée choisie, 24 h au plus).
func (s *Service) Approve(ctx context.Context, org, id uuid.UUID, by Person, hours int) (Grant, error) {
	var out Grant
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		g, err := lockGrant(tx, org, id)
		if err != nil {
			return err
		}
		if g.Status != StatusRequested {
			return ErrNotPending
		}
		if hours == 0 {
			hours = g.RequestedHours
		}
		if hours, err = checkHours(hours); err != nil {
			return err
		}
		now := s.now()
		exp := now.Add(time.Duration(hours) * time.Hour)
		g.Status, g.DecidedBy, g.DecidedEmail, g.DecidedAt, g.ExpiresAt = StatusActive, &by.ID, by.Email, &now, &exp
		if err := tx.Save(&g).Error; err != nil {
			return err
		}
		out = g
		return bind(ctx, tx, g, by.principal())
	})
	if err != nil {
		return Grant{}, err
	}
	s.journal(ctx, by.principal(), "organization.assistance.granted", out, nil)
	s.notify(ctx, org, NoticeGranted, out.StaffEmail, out.Reason, out.ExpiresAt)
	return out, nil
}

// GrantForTicket : un propriétaire ouvre l'accès de lui-même, à l'agent
// assigné au ticket.
func (s *Service) GrantForTicket(ctx context.Context, org, ticket uuid.UUID, by Person, reason string, hours int) (Grant, error) {
	reason, err := checkReason(reason)
	if err != nil {
		return Grant{}, err
	}
	if hours, err = checkHours(hours); err != nil {
		return Grant{}, err
	}
	var t struct {
		OrganizationID   uuid.UUID
		AssignedToUserID *uuid.UUID
	}
	if err := s.DB.WithContext(ctx).Raw(`SELECT organization_id, assigned_to_user_id FROM support_tickets WHERE id = ?`, ticket).
		Scan(&t).Error; err != nil {
		return Grant{}, err
	}
	if t.OrganizationID != org {
		return Grant{}, betoerrors.New(betoerrors.CodeNotFound, "ticket not found")
	}
	if t.AssignedToUserID == nil {
		return Grant{}, ErrNoAssignee
	}
	var email string
	if err := s.DB.WithContext(ctx).Raw(`SELECT email FROM users WHERE id = ?`, *t.AssignedToUserID).Scan(&email).Error; err != nil {
		return Grant{}, err
	}
	now := s.now()
	exp := now.Add(time.Duration(hours) * time.Hour)
	g := Grant{ID: uuid.New(), OrganizationID: org, TicketID: &ticket, StaffUserID: *t.AssignedToUserID, StaffEmail: email,
		Mode: ModeConsent, Status: StatusActive, Reason: reason, RequestedHours: hours, RequestedAt: now,
		DecidedBy: &by.ID, DecidedEmail: by.Email, DecidedAt: &now, ExpiresAt: &exp}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := insertTx(tx, &g); err != nil {
			return err
		}
		return bind(ctx, tx, g, by.principal())
	})
	if err != nil {
		return Grant{}, err
	}
	s.journal(ctx, by.principal(), "organization.assistance.granted", g, nil)
	s.notify(ctx, org, NoticeGranted, g.StaffEmail, reason, &exp)
	return g, nil
}

// Deny : un propriétaire refuse une demande.
func (s *Service) Deny(ctx context.Context, org, id uuid.UUID, by Person, note string) (Grant, error) {
	return s.end(ctx, org, id, by.principal(), by.Email, note, StatusRequested, StatusDenied, "organization.assistance.denied")
}

// Revoke : un propriétaire retire un accès actif, effet immédiat.
func (s *Service) Revoke(ctx context.Context, org, id uuid.UUID, by Person, note string) (Grant, error) {
	return s.end(ctx, org, id, by.principal(), by.Email, note, StatusActive, StatusRevoked, "organization.assistance.revoked")
}

// Close : la personne de l'équipe clôt son accès (ou retire sa demande).
func (s *Service) Close(ctx context.Context, staff Person, id uuid.UUID, note string) (Grant, error) {
	var g Grant
	if err := s.DB.WithContext(ctx).Where("id = ? AND staff_user_id = ?", id, staff.ID).First(&g).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Grant{}, ErrNotFound
		}
		return Grant{}, err
	}
	from := g.Status
	if from != StatusActive && from != StatusRequested {
		return Grant{}, ErrNotActive
	}
	return s.end(ctx, g.OrganizationID, id, staff.principal(), staff.Email, note, from, StatusClosed, "organization.assistance.closed")
}

// end : passage d'un statut ouvert à un statut final ; liaison retirée.
func (s *Service) end(ctx context.Context, org, id uuid.UUID, actor, actorEmail, note, from, to, action string) (Grant, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxReason {
		return Grant{}, betoerrors.New(betoerrors.CodeInvalidInput, "the note is limited to 500 characters")
	}
	var out Grant
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		g, err := lockGrant(tx, org, id)
		if err != nil {
			return err
		}
		if g.Status != from {
			if from == StatusRequested {
				return ErrNotPending
			}
			return ErrNotActive
		}
		now := s.now()
		by := actorEmail
		if by == "" {
			by = actor
		}
		g.Status, g.EndedAt, g.EndedBy, g.EndNote = to, &now, by, note
		if err := tx.Save(&g).Error; err != nil {
			return err
		}
		out = g
		if from == StatusActive {
			return unbind(ctx, tx, g, actor)
		}
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	iam.NotifyChanged(iam.UserPrincipal(out.StaffUserID))
	s.journal(ctx, actor, action, out, map[string]any{"note": note})
	return out, nil
}

// List : les attributions d'une organisation, récentes d'abord.
func (s *Service) List(ctx context.Context, org uuid.UUID) ([]Grant, error) {
	if err := s.Reap(ctx); err != nil {
		return nil, err
	}
	var out []Grant
	err := s.DB.WithContext(ctx).Where("organization_id = ?", org).Order("requested_at DESC").Limit(200).Find(&out).Error
	return out, err
}

// ListStaff : les attributions d'une personne de l'équipe (nil : toutes),
// ouvertes d'abord.
func (s *Service) ListStaff(ctx context.Context, staff *uuid.UUID) ([]Grant, error) {
	if err := s.Reap(ctx); err != nil {
		return nil, err
	}
	q := s.DB.WithContext(ctx).Model(&Grant{})
	if staff != nil {
		q = q.Where("staff_user_id = ?", *staff)
	}
	var out []Grant
	err := q.Order("CASE WHEN status IN ('requested','active') THEN 0 ELSE 1 END, requested_at DESC").Limit(200).Find(&out).Error
	return out, err
}

// Active : la personne a un accès d'assistance actif sur l'organisation.
func (s *Service) Active(ctx context.Context, staff, org uuid.UUID) (*Grant, error) {
	var g Grant
	err := s.DB.WithContext(ctx).Where("organization_id = ? AND staff_user_id = ? AND status = ? AND expires_at > ?",
		org, staff, StatusActive, s.now()).First(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &g, err
}

// HasActive : la personne a au moins un accès d'assistance actif (catalogue
// de l'Opérateur : outils sur les ressources d'un client).
func (s *Service) HasActive(ctx context.Context, staff uuid.UUID) (bool, error) {
	var n int64
	err := s.DB.WithContext(ctx).Model(&Grant{}).Where("staff_user_id = ? AND status = ? AND expires_at > ?",
		staff, StatusActive, s.now()).Limit(1).Count(&n).Error
	return n > 0, err
}

// Reap : accès échus et demandes sans réponse expirés ; liaisons sans
// attribution active retirées (restauration, édition à la main).
func (s *Service) Reap(ctx context.Context) error {
	now := s.now()
	var due []Grant
	if err := s.DB.WithContext(ctx).Where("(status = ? AND expires_at <= ?) OR (status = ? AND requested_at <= ?)",
		StatusActive, now, StatusRequested, now.Add(-RequestTTL)).Limit(500).Find(&due).Error; err != nil {
		return err
	}
	for _, g := range due {
		if _, err := s.end(ctx, g.OrganizationID, g.ID, systemActor, "", "", g.Status, StatusExpired, "organization.assistance.expired"); err != nil &&
			!errors.Is(err, ErrNotActive) && !errors.Is(err, ErrNotPending) {
			return err
		}
	}
	return s.reconcile(ctx)
}

const systemActor = "system:assistance"

func (s *Service) reconcile(ctx context.Context) error {
	bindings, err := iam.ReservedBindings(ctx, s.DB, iam.RoleSupportAssistant)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		org, okOrg := b.Resource.Organization()
		kind, user, perr := b.Principal.Parse()
		if okOrg && perr == nil && kind == iam.KindUser {
			g, err := s.Active(ctx, user, org)
			if err != nil {
				return err
			}
			if g != nil {
				continue
			}
		}
		removed, err := iam.RemoveRole(ctx, s.DB, b.Resource, b.Role, b.Principal, systemActor)
		if err != nil {
			return err
		}
		if removed {
			iam.NotifyChanged(b.Principal)
			s.write(ctx, &audit.Entry{Principal: systemActor, AuthMethod: "system", Action: "organization.assistance.reconciled",
				Resource: string(b.Resource), Decision: audit.DecisionAllow, Status: 200,
				Metadata: map[string]any{"principal": string(b.Principal), "reason": "binding without an active assistance access"}})
		}
	}
	return nil
}

// RunReaper : Reap au démarrage puis chaque minute.
func (s *Service) RunReaper(ctx context.Context, logErr func(error)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.Reap(ctx); err != nil && ctx.Err() == nil && logErr != nil {
			logErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) insert(ctx context.Context, g *Grant) error {
	return insertTx(s.DB.WithContext(ctx), g)
}

func insertTx(tx *gorm.DB, g *Grant) error {
	err := tx.Create(g).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) || (err != nil && strings.Contains(err.Error(), "assistance_grants_open")) {
		return ErrAlreadyOpen
	}
	return err
}

func lockGrant(tx *gorm.DB, org, id uuid.UUID) (Grant, error) {
	var g Grant
	err := tx.Set("gorm:query_option", "FOR UPDATE").Raw(`SELECT * FROM assistance_grants WHERE id = ? AND organization_id = ? FOR UPDATE`, id, org).
		Scan(&g).Error
	if err != nil {
		return Grant{}, err
	}
	if g.ID == uuid.Nil {
		return Grant{}, ErrNotFound
	}
	return g, nil
}

// condition : l'échéance portée par la liaison.
func condition(exp time.Time) *iam.Condition {
	return &iam.Condition{Title: "assistance", Description: "Accès d'assistance betorun (ADR 0017)",
		Expression: fmt.Sprintf("request.time < timestamp('%s')", exp.UTC().Format(time.RFC3339))}
}

func bind(ctx context.Context, tx *gorm.DB, g Grant, actor string) error {
	// Une liaison laissée par un accès précédent (échéance passée) est remplacée.
	if _, err := iam.RemoveRole(ctx, tx, iam.OrganizationName(g.OrganizationID), iam.RoleSupportAssistant, iam.UserPrincipal(g.StaffUserID), actor); err != nil {
		return err
	}
	return iam.AddBinding(ctx, tx, iam.Binding{Resource: iam.OrganizationName(g.OrganizationID), Role: iam.RoleSupportAssistant,
		Principal: iam.UserPrincipal(g.StaffUserID), Condition: condition(*g.ExpiresAt)}, actor)
}

func unbind(ctx context.Context, tx *gorm.DB, g Grant, actor string) error {
	_, err := iam.RemoveRole(ctx, tx, iam.OrganizationName(g.OrganizationID), iam.RoleSupportAssistant, iam.UserPrincipal(g.StaffUserID), actor)
	return err
}

// journal : entrée du journal de l'organisation (lisible par le client).
func (s *Service) journal(ctx context.Context, actor, action string, g Grant, extra map[string]any) {
	md := map[string]any{"assistance_id": g.ID.String(), "staff": g.StaffEmail, "mode": g.Mode, "status": g.Status, "reason": g.Reason}
	if g.TicketID != nil {
		md["ticket_id"] = g.TicketID.String()
	}
	if g.ExpiresAt != nil {
		md["expires_at"] = g.ExpiresAt.Format(time.RFC3339)
	}
	for k, v := range extra {
		if v != "" {
			md[k] = v
		}
	}
	method := "session"
	if actor == systemActor {
		method = "system"
	}
	s.write(ctx, &audit.Entry{Principal: actor, AuthMethod: method, Action: action, Permission: string(iam.OrganizationsGrantAssistance),
		Resource: string(iam.OrganizationName(g.OrganizationID)), Decision: audit.DecisionAllow, Status: 200, Metadata: md})
}

func (s *Service) write(ctx context.Context, e *audit.Entry) {
	if s.Audit == nil {
		return
	}
	_ = s.Audit.Write(context.WithoutCancel(ctx), e)
}

func (s *Service) notify(ctx context.Context, org uuid.UUID, kind, staff, reason string, until *time.Time) {
	if s.Notify != nil {
		s.Notify.AssistanceNotice(ctx, org, kind, staff, reason, until)
	}
}
