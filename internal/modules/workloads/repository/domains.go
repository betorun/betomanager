package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/core/pgerr"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// DomainStore : domaines personnalisés des conteneurs.
type DomainStore interface {
	Create(ctx context.Context, d *domain.WorkloadDomain) error
	List(ctx context.Context, workload uuid.UUID) ([]domain.WorkloadDomain, error)
	Get(ctx context.Context, workload, id uuid.UUID) (*domain.WorkloadDomain, error)
	Delete(ctx context.Context, workload, id uuid.UUID) error
	// VerifiedRoutes : noms et chemins à router pour ce conteneur.
	VerifiedRoutes(ctx context.Context, workload uuid.UUID) ([]domain.Route, error)
	// HostClaims : domaines vérifiés portant ce nom (tous chemins), hors du
	// domaine donné, avec le projet de leur conteneur.
	HostClaims(ctx context.Context, hostname string, except uuid.UUID) ([]domain.HostClaim, error)
	// ProjectHostToken : preuve d'un nom déjà demandé dans ce projet (un autre
	// chemin) : vérifiée de préférence, sinon la plus ancienne en attente ; ""
	// si aucune. verified : la preuve est déjà validée.
	ProjectHostToken(ctx context.Context, project uuid.UUID, hostname string) (token string, verified bool, err error)
	// HostnameTaken : nom déjà réservé par un site statique vérifié.
	SiteHostnameTaken(ctx context.Context, hostname string) (bool, error)
	// SetVerified : ErrDomainClaimed si le nom a été pris entre-temps.
	SetVerified(ctx context.Context, id uuid.UUID, verified bool) error
	Touch(ctx context.Context, id uuid.UUID) error
}

type gormDomainStore struct{ db *gorm.DB }

// NewDomainStore : stockage en base.
func NewDomainStore(db *gorm.DB) DomainStore { return &gormDomainStore{db: db} }

func (s *gormDomainStore) Create(ctx context.Context, d *domain.WorkloadDomain) error {
	err := s.db.WithContext(ctx).Create(d).Error
	if isUniqueViolation(err) {
		return domain.ErrDomainExists
	}
	return err
}

func (s *gormDomainStore) List(ctx context.Context, workload uuid.UUID) ([]domain.WorkloadDomain, error) {
	out := []domain.WorkloadDomain{}
	return out, s.db.WithContext(ctx).Where("workload_id = ?", workload).Order("hostname, path").Find(&out).Error
}

func (s *gormDomainStore) Get(ctx context.Context, workload, id uuid.UUID) (*domain.WorkloadDomain, error) {
	var d domain.WorkloadDomain
	err := s.db.WithContext(ctx).Where("workload_id = ? AND id = ?", workload, id).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDomainNotFound
	}
	return &d, err
}

func (s *gormDomainStore) Delete(ctx context.Context, workload, id uuid.UUID) error {
	return s.db.WithContext(ctx).Where("workload_id = ? AND id = ?", workload, id).Delete(&domain.WorkloadDomain{}).Error
}

func (s *gormDomainStore) VerifiedRoutes(ctx context.Context, workload uuid.UUID) ([]domain.Route, error) {
	out := []domain.Route{}
	err := s.db.WithContext(ctx).Model(&domain.WorkloadDomain{}).Select("hostname AS host, path").
		Where("workload_id = ? AND verified", workload).Order("hostname, path").Scan(&out).Error
	return out, err
}

func (s *gormDomainStore) HostClaims(ctx context.Context, hostname string, except uuid.UUID) ([]domain.HostClaim, error) {
	var rows []domain.WorkloadDomain
	err := s.db.WithContext(ctx).Where("hostname = ? AND verified AND id <> ?", hostname, except).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.WorkloadID)
	}
	var owners []struct {
		ID        uuid.UUID
		ProjectID uuid.UUID
	}
	if err := s.db.WithContext(ctx).Table("workloads").Select("id, project_id").Where("id IN ?", ids).Scan(&owners).Error; err != nil {
		return nil, err
	}
	project := make(map[uuid.UUID]uuid.UUID, len(owners))
	for _, o := range owners {
		project[o.ID] = o.ProjectID
	}
	out := make([]domain.HostClaim, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.HostClaim{WorkloadDomain: r, ProjectID: project[r.WorkloadID]})
	}
	return out, nil
}

func (s *gormDomainStore) ProjectHostToken(ctx context.Context, project uuid.UUID, hostname string) (string, bool, error) {
	var d domain.WorkloadDomain
	err := s.db.WithContext(ctx).Table("workload_domains AS d").Select("d.*").
		Joins("JOIN workloads w ON w.id = d.workload_id").
		Where("w.project_id = ? AND d.hostname = ?", project, hostname).
		Order("d.verified DESC, d.created_at").First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	return d.Token, d.Verified, err
}

func (s *gormDomainStore) SiteHostnameTaken(ctx context.Context, hostname string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Table("site_domains").Where("hostname = ? AND verified", hostname).Count(&n).Error
	return n > 0, err
}

func (s *gormDomainStore) SetVerified(ctx context.Context, id uuid.UUID, verified bool) error {
	now := time.Now()
	updates := map[string]any{"verified": verified, "checked_at": now, "verified_at": nil}
	if verified {
		updates["verified_at"] = now
	}
	err := s.db.WithContext(ctx).Model(&domain.WorkloadDomain{}).Where("id = ?", id).Updates(updates).Error
	if isUniqueViolation(err) {
		return domain.ErrDomainClaimed
	}
	return err
}

func (s *gormDomainStore) Touch(ctx context.Context, id uuid.UUID) error {
	return s.db.WithContext(ctx).Model(&domain.WorkloadDomain{}).Where("id = ?", id).Update("checked_at", time.Now()).Error
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerr.UniqueViolation
}
