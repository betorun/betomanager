// Package repository : réserve d'IP de sortie (Postgres) et passerelle Cilium.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
)

// Store : réserve d'IP de sortie.
type Store struct{ DB *gorm.DB }

// ProjectPlace : namespace et cluster d'un projet vivant, et la passerelle
// de ce cluster.
type ProjectPlace struct {
	Namespace string
	ClusterID string
	Gateway   string
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func foreignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

// ErrUnknownCluster : la réserve vise un cluster absent du registre.
var ErrUnknownCluster = errors.New("unknown cluster")

// List : IP d'un cluster, avec le nom du projet qui les tient.
func (s *Store) List(ctx context.Context, clusterID string) ([]domain.AdminAddress, error) {
	var rows []struct {
		domain.Address
		ProjectName *string
	}
	err := s.DB.WithContext(ctx).Raw(`SELECT e.*, p.name AS project_name FROM egress_addresses e
		LEFT JOIN projects p ON p.id = e.project_id
		WHERE e.cluster_id = ? ORDER BY e.address`, clusterID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]domain.AdminAddress, 0, len(rows))
	for _, r := range rows {
		a := domain.AdminAddress{Address: r.Address, Assigned: r.ProjectID != nil}
		if r.ProjectName != nil {
			a.ProjectName = *r.ProjectName
		}
		out = append(out, a)
	}
	return out, nil
}

// Declare : nouvelle IP libre.
func (s *Store) Declare(ctx context.Context, a *domain.Address) error {
	err := s.DB.WithContext(ctx).Create(a).Error
	switch {
	case uniqueViolation(err):
		return domain.ErrAddressExists
	case foreignKeyViolation(err):
		return ErrUnknownCluster
	}
	return err
}

// Delete : retire une IP libre de la réserve.
func (s *Store) Delete(ctx context.Context, clusterID string, id uuid.UUID) error {
	var a domain.Address
	err := s.DB.WithContext(ctx).Where("id = ? AND cluster_id = ?", id, clusterID).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrAddressNotFound
	}
	if err != nil {
		return err
	}
	res := s.DB.WithContext(ctx).Exec(`DELETE FROM egress_addresses WHERE id = ? AND project_id IS NULL`, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrAddressInUse
	}
	return nil
}

// ForProject : IP du projet (nil : aucune).
func (s *Store) ForProject(ctx context.Context, project uuid.UUID) (*domain.Address, error) {
	var a domain.Address
	err := s.DB.WithContext(ctx).Where("project_id = ?", project).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

// FreeCount : IP libres d'un cluster.
func (s *Store) FreeCount(ctx context.Context, clusterID string) (int, error) {
	var n int64
	err := s.DB.WithContext(ctx).Model(&domain.Address{}).Where("cluster_id = ? AND project_id IS NULL", clusterID).Count(&n).Error
	return int(n), err
}

// Assign : une IP libre du cluster attribuée au projet (deux demandes
// simultanées n'obtiennent jamais la même : SKIP LOCKED ; un projet n'en a
// qu'une : index unique). nil : aucune libre.
// only : adresses permises (nil : toutes ; vide : aucune).
func (s *Store) Assign(ctx context.Context, clusterID string, project uuid.UUID, by string, now time.Time, only []string) (*domain.Address, error) {
	if only != nil && len(only) == 0 {
		return nil, nil
	}
	filter, args := "", []any{project, now, by, now, clusterID}
	if only != nil {
		filter = " AND address = ANY(?)"
		args = append(args, pq.Array(only))
	}
	var rows []domain.Address
	err := s.DB.WithContext(ctx).Raw(`UPDATE egress_addresses SET project_id = ?, assigned_at = ?, assigned_by = ?, updated_at = ?
		WHERE id = (SELECT id FROM egress_addresses WHERE cluster_id = ? AND project_id IS NULL`+filter+`
		            ORDER BY address LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING *`, args...).Scan(&rows).Error
	if uniqueViolation(err) {
		return nil, domain.ErrAlreadyHasAddress
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// Release : l'IP revient dans la réserve.
func (s *Store) Release(ctx context.Context, id uuid.UUID, now time.Time) error {
	return s.DB.WithContext(ctx).Exec(`UPDATE egress_addresses SET project_id = NULL, assigned_at = NULL, assigned_by = '', updated_at = ?
		WHERE id = ?`, now, id).Error
}

// Place : namespace, cluster (vide : défaut) et passerelle d'un projet vivant.
func (s *Store) Place(ctx context.Context, project uuid.UUID, defaultCluster string) (*ProjectPlace, error) {
	var rows []ProjectPlace
	err := s.DB.WithContext(ctx).Raw(`SELECT p.namespace, COALESCE(p.cluster_id, ?) AS cluster_id, COALESCE(c.egress_gateway, '') AS gateway
		FROM projects p LEFT JOIN clusters c ON c.cluster_id = COALESCE(p.cluster_id, ?)
		WHERE p.id = ? AND p.deleted_at IS NULL`, defaultCluster, defaultCluster, project).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// Clusters : clusters qui ont une réserve.
func (s *Store) Clusters(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.DB.WithContext(ctx).Raw(`SELECT DISTINCT cluster_id FROM egress_addresses ORDER BY cluster_id`).Scan(&ids).Error
	return ids, err
}

// Orphan : IP tenue par un projet supprimé, avec où retirer sa règle.
type Orphan struct {
	domain.Address
	Namespace string
}

// Orphans : IP de projets supprimés (à rendre).
func (s *Store) Orphans(ctx context.Context) ([]Orphan, error) {
	var rows []Orphan
	err := s.DB.WithContext(ctx).Raw(`SELECT e.*, p.namespace FROM egress_addresses e
		JOIN projects p ON p.id = e.project_id WHERE p.deleted_at IS NOT NULL`).Scan(&rows).Error
	return rows, err
}
