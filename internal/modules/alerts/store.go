// Package alerts : alertes email minimales (conteneur tombé ou qui redémarre en
// boucle, build échoué). Voir docs/adr/0009-alertes-email.md.
package alerts

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Recipient : destinataire d'une alerte.
type Recipient struct {
	Email    string
	FullName string
}

// Store : limitation de fréquence et destinataires.
type Store interface {
	// Claim : true si aucune alerte de ce type n'est partie pour ce sujet
	// depuis `every` ; l'envoi est alors réservé (atomique entre instances).
	Claim(ctx context.Context, subject uuid.UUID, kind string, every time.Duration) (bool, error)
	// Recipients : owners du projet (comptes actifs).
	Recipients(ctx context.Context, project uuid.UUID) ([]Recipient, error)
	// ProjectName : nom affiché du projet.
	ProjectName(ctx context.Context, project uuid.UUID) (string, error)
	// OrgRecipients : owners de l'organisation (comptes actifs).
	OrgRecipients(ctx context.Context, org uuid.UUID) ([]Recipient, error)
	// OrgName : nom affiché de l'organisation.
	OrgName(ctx context.Context, org uuid.UUID) (string, error)
}

// maxRecipients : au-delà, les autres owners ne sont pas prévenus.
const maxRecipients = 20

type gormStore struct {
	db *gorm.DB
}

// NewStore : stockage en base.
func NewStore(db *gorm.DB) Store {
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

func (s *gormStore) Recipients(ctx context.Context, project uuid.UUID) ([]Recipient, error) {
	var out []Recipient
	err := s.db.WithContext(ctx).Raw(`
		SELECT u.email, u.full_name FROM iam_bindings b
		JOIN users u ON b.principal = ? || u.id::text
		WHERE b.resource_name = ? AND b.role = ? AND u.deleted_at IS NULL AND u.is_active
		ORDER BY u.email LIMIT ?`,
		iam.KindUser+iam.PrincipalSeparator, string(iam.ProjectName(project)), iam.RoleOwner, maxRecipients).
		Scan(&out).Error
	return out, err
}

func (s *gormStore) ProjectName(ctx context.Context, project uuid.UUID) (string, error) {
	var name string
	err := s.db.WithContext(ctx).Raw(`SELECT name FROM projects WHERE id = ?`, project).Scan(&name).Error
	return name, err
}

func (s *gormStore) OrgRecipients(ctx context.Context, org uuid.UUID) ([]Recipient, error) {
	var out []Recipient
	err := s.db.WithContext(ctx).Raw(`
		SELECT u.email, u.full_name FROM iam_bindings b
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
