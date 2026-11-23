package repository

import (
	"context"
	"errors"
	"time"

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
