package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// RegistryStore : registre des clusters et leur santé (Postgres).
type RegistryStore struct{ DB *gorm.DB }

// isUnique : violation d'unicité Postgres (23505).
func isUnique(err error) bool {
	return err != nil && (errors.Is(err, gorm.ErrDuplicatedKey) || containsCode(err, "23505"))
}

func containsCode(err error, code string) bool {
	type sqlState interface{ SQLState() string }
	var s sqlState
	if errors.As(err, &s) {
		return s.SQLState() == code
	}
	return false
}

func (s *RegistryStore) List(ctx context.Context) ([]domain.Cluster, error) {
	out := []domain.Cluster{}
	return out, s.DB.WithContext(ctx).Order("is_default DESC, region, cluster_id").Find(&out).Error
}

// Get : ErrClusterNotFound s'il n'existe pas.
func (s *RegistryStore) Get(ctx context.Context, clusterID string) (*domain.Cluster, error) {
	var c domain.Cluster
	err := s.DB.WithContext(ctx).Where("cluster_id = ?", clusterID).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrClusterNotFound
	}
	return &c, err
}

// Declare : ErrClusterExists si l'identifiant est pris (déclaré ou enrôlé).
func (s *RegistryStore) Declare(ctx context.Context, c *domain.Cluster) error {
	err := s.DB.WithContext(ctx).Create(c).Error
	if isUnique(err) {
		return domain.ErrClusterExists
	}
	return err
}

// Update : champs du registre.
func (s *RegistryStore) Update(ctx context.Context, clusterID string, fields map[string]any) error {
	fields["updated_at"] = time.Now().UTC()
	res := s.DB.WithContext(ctx).Model(&domain.Cluster{}).Where("cluster_id = ?", clusterID).Updates(fields)
	if res.Error == nil && res.RowsAffected == 0 {
		return domain.ErrClusterNotFound
	}
	return res.Error
}

// SetDefault : ce cluster devient le seul par défaut.
func (s *RegistryStore) SetDefault(ctx context.Context, clusterID string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&domain.Cluster{}).Where("is_default").Update("is_default", false).Error; err != nil {
			return err
		}
		res := tx.Model(&domain.Cluster{}).Where("cluster_id = ?", clusterID).
			Updates(map[string]any{"is_default": true, "updated_at": time.Now().UTC()})
		if res.Error == nil && res.RowsAffected == 0 {
			return domain.ErrClusterNotFound
		}
		return res.Error
	})
}

// EnsureDefault : le cluster de la configuration du déploiement existe au
// registre ; il devient le défaut s'il n'y en a aucun. Rien d'autre ne change.
func (s *RegistryStore) EnsureDefault(ctx context.Context, clusterID string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&domain.Cluster{ClusterID: clusterID, Name: clusterID, DeclaredBy: "system:config"}).Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&domain.Cluster{}).Where("is_default").Count(&n).Error; err != nil || n > 0 {
			return err
		}
		return tx.Model(&domain.Cluster{}).Where("cluster_id = ?", clusterID).Update("is_default", true).Error
	})
}

// Delete : le relevé de santé part avec lui.
func (s *RegistryStore) Delete(ctx context.Context, clusterID string) error {
	res := s.DB.WithContext(ctx).Where("cluster_id = ? AND NOT is_default", clusterID).Delete(&domain.Cluster{})
	if res.Error == nil && res.RowsAffected == 0 {
		return domain.ErrClusterNotFound
	}
	return res.Error
}

// Healths : derniers relevés, par cluster.
func (s *RegistryStore) Healths(ctx context.Context) (map[string]*domain.Health, error) {
	var rows []domain.Health
	if err := s.DB.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*domain.Health, len(rows))
	for i := range rows {
		out[rows[i].ClusterID] = &rows[i]
	}
	return out, nil
}

// SaveHealth : remplace le relevé du cluster.
func (s *RegistryStore) SaveHealth(ctx context.Context, h *domain.Health) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(h).Error
}

// ProjectCounts : projets (non supprimés) par cluster ; sans cluster : sous
// defaultID (le cluster par défaut les porte).
func (s *RegistryStore) ProjectCounts(ctx context.Context, defaultID string) (map[string]int, error) {
	var rows []struct {
		ClusterID string
		N         int
	}
	if err := s.DB.WithContext(ctx).Raw(`SELECT COALESCE(cluster_id, ?) AS cluster_id, COUNT(*) AS n
		FROM projects WHERE deleted_at IS NULL GROUP BY 1`, defaultID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.ClusterID] = r.N
	}
	return out, nil
}

// ProjectCluster : cluster d'un projet, supprimé compris ("" : défaut).
func (s *RegistryStore) ProjectCluster(ctx context.Context, project uuid.UUID) (string, bool, error) {
	return s.lookup(ctx, "id = ?", project)
}

// NamespaceCluster : cluster du projet de ce namespace, supprimé compris.
func (s *RegistryStore) NamespaceCluster(ctx context.Context, namespace string) (string, bool, error) {
	return s.lookup(ctx, "namespace = ?", namespace)
}

func (s *RegistryStore) lookup(ctx context.Context, where string, arg any) (string, bool, error) {
	var rows []struct{ ClusterID *string }
	if err := s.DB.WithContext(ctx).Raw(`SELECT cluster_id FROM projects WHERE `+where+` ORDER BY deleted_at NULLS FIRST LIMIT 1`, arg).
		Scan(&rows).Error; err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	if rows[0].ClusterID == nil {
		return "", true, nil
	}
	return *rows[0].ClusterID, true, nil
}
