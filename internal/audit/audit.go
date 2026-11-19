// Package audit : journal d'audit immuable (docs/architecture/iam.md §11).
//
// Capture : un middleware global journalise toute écriture d'un principal
// authentifié et tout refus d'autorisation. Les services enrichissent l'entrée
// en cours via Annotate (ex. setIamPolicy : bindings ajoutés et retirés).
// Jamais de corps de requête ni de secret dans le journal.
package audit

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	DecisionAllow = "ALLOW"
	DecisionDeny  = "DENY"
)

// Entry : une ligne du journal.
type Entry struct {
	ID         int64          `gorm:"primaryKey" json:"id"`
	At         time.Time      `json:"at"`
	Principal  string         `json:"principal"`
	AuthMethod string         `json:"auth_method,omitempty"`
	APIKeyID   string         `json:"api_key_id,omitempty"`
	Action     string         `json:"action"`
	Permission string         `json:"permission,omitempty"`
	Resource   string         `json:"resource,omitempty"`
	ProjectID  *uuid.UUID     `gorm:"type:uuid" json:"project_id,omitempty"`
	Decision   string         `json:"decision"`
	Status     int            `json:"status"`
	ClientIP   string         `json:"client_ip,omitempty"`
	UserAgent  string         `json:"user_agent,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	Metadata   map[string]any `gorm:"-" json:"metadata,omitempty"`
	MetadataDB []byte         `gorm:"column:metadata;type:jsonb" json:"-"`
}

func (Entry) TableName() string { return "audit_log" }

// Writer : destination des entrées.
type Writer interface {
	Write(ctx context.Context, e *Entry) error
}

// GormWriter : écriture en base (table append-only).
type GormWriter struct{ DB *gorm.DB }

func (w *GormWriter) Write(ctx context.Context, e *Entry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	raw, err := json.Marshal(e.Metadata)
	if err != nil || e.Metadata == nil {
		raw = []byte(emptyMetadata)
	}
	e.MetadataDB = raw
	return w.DB.WithContext(ctx).Create(e).Error
}

// ── Annotations depuis les services ──────────────────────────────────────

type recorderKey struct{}

// Recorder : métadonnées de l'entrée en cours, alimentées par les services.
type Recorder struct {
	mu   sync.Mutex
	meta map[string]any
}

// WithRecorder : contexte portant un recorder (posé par le middleware).
func WithRecorder(ctx context.Context) (context.Context, *Recorder) {
	r := &Recorder{meta: map[string]any{}}
	return context.WithValue(ctx, recorderKey{}, r), r
}

// Annotate ajoute une métadonnée à l'entrée d'audit de la requête en cours
// (no-op hors requête auditée). Ne jamais y mettre de secret.
func Annotate(ctx context.Context, key string, value any) {
	r, ok := ctx.Value(recorderKey{}).(*Recorder)
	if !ok {
		return
	}
	r.mu.Lock()
	r.meta[key] = value
	r.mu.Unlock()
}

// Metadata : copie des annotations.
func (r *Recorder) Metadata() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(r.meta))
	for k, v := range r.meta {
		out[k] = v
	}
	return out
}

// Query : lecture du journal, la plus récente d'abord, pagination par curseur.
type Query struct {
	// Resource : entrées sur cette ressource et ses descendantes.
	Resource string
	// ProjectID : en plus, toutes les entrées du projet et les actions
	// d'organisation qui le touchent (voir projectScope).
	ProjectID *uuid.UUID
	// OrganizationID : actions sur l'organisation elle-même (accès,
	// groupes, rôles personnalisés, projets créés) ; pas celles des projets.
	OrganizationID *uuid.UUID
	BeforeID       int64
	Limit          int
}

// MaxPageSize : taille maximale d'une page.
const MaxPageSize = 200

// DefaultPageSize : taille d'une page si non précisée.
const DefaultPageSize = 50

// emptyMetadata : métadonnées enregistrées quand il n'y en a pas.
const emptyMetadata = "{}"

// orgSetIamPolicy : permission d'un changement des accès de l'organisation
// (pas d'import du paquet iam : il dépend de celui-ci).
const orgSetIamPolicy = "resourcemanager.organizations.setIamPolicy"

// projectScope : journal d'un projet. En plus de ses propres entrées, les
// actions d'organisation qui le touchent (décision du 2026-11-19) :
//   - les changements d'accès de l'organisation (hérités par le projet) ;
//   - les groupes qui ont un rôle sur le projet ou sur l'organisation
//     (création, membres ajoutés ou retirés) ;
//   - les rôles personnalisés d'organisation attribués dans le projet.
//
// Groupes et rôles sont lus dans les bindings actuels : un groupe retiré du
// projet sort aussi de son journal.
const projectScope = `project_id = @pid OR resource = @r OR resource LIKE @rl
	OR (permission = @orgSetPolicy AND resource = (SELECT 'organizations/' || organization_id FROM projects WHERE id = @pid))
	OR resource IN (
		SELECT 'organizations/' || p.organization_id || '/groups/' || substr(b.principal, 7)
		FROM iam_bindings b JOIN projects p ON p.id = @pid
		WHERE b.principal LIKE 'group:%'
		  AND (b.resource_name = @r OR b.resource_name LIKE @rl OR b.resource_name = 'organizations/' || p.organization_id))
	OR resource IN (
		SELECT b.role FROM iam_bindings b
		WHERE b.role LIKE 'organizations/%' AND (b.resource_name = @r OR b.resource_name LIKE @rl))`

// List lit le journal selon q.
func List(ctx context.Context, db *gorm.DB, q Query) ([]Entry, error) {
	limit := q.Limit
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}
	tx := db.WithContext(ctx).Model(&Entry{})
	switch {
	case q.OrganizationID != nil:
		org := "organizations/" + q.OrganizationID.String()
		tx = tx.Where("resource = ? OR resource LIKE ?", org, org+"/%")
	case q.ProjectID != nil:
		tx = tx.Where(projectScope, map[string]any{"pid": *q.ProjectID, "r": q.Resource, "rl": q.Resource + "/%",
			"orgSetPolicy": orgSetIamPolicy})
	default:
		tx = tx.Where("resource = ? OR resource LIKE ?", q.Resource, q.Resource+"/%")
	}
	if q.BeforeID > 0 {
		tx = tx.Where("id < ?", q.BeforeID)
	}
	var out []Entry
	if err := tx.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		if len(out[i].MetadataDB) > 2 {
			_ = json.Unmarshal(out[i].MetadataDB, &out[i].Metadata)
		}
	}
	return out, nil
}
