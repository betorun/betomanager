package repository

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// IncidentStatuses : statuts posés par un événement de pod (clusters.OnPodEvent).
var IncidentStatuses = []string{
	utils.WorkloadCrashLoop,
	utils.WorkloadImagePullFailed,
	utils.WorkloadConfigError,
	utils.WorkloadOOMKilled,
	utils.WorkloadEvicted,
	utils.WorkloadFailed,
}

// IncidentStore : conteneurs en incident signalé par le cluster, et leur retour
// à RUNNING.
type IncidentStore interface {
	// ListIncidents : statut d'incident et phase POD_* (un FAILED écrit par un
	// déploiement n'en fait pas partie : seul un nouveau déploiement le lève).
	ListIncidents(ctx context.Context, limit int) ([]domain.Workload, error)
	// Recover : RUNNING si le statut et la phase n'ont pas changé depuis la
	// lecture (un déploiement lancé entre-temps garde la main). false : rien
	// n'a été écrit.
	Recover(ctx context.Context, id uuid.UUID, status, phase, newPhase string) (bool, error)
}

type incidentStore struct {
	db *gorm.DB
}

// NewIncidentStore : stockage en base.
func NewIncidentStore(db *gorm.DB) IncidentStore {
	return &incidentStore{db: db}
}

func (s *incidentStore) ListIncidents(ctx context.Context, limit int) ([]domain.Workload, error) {
	var rows []domain.Workload
	err := s.db.WithContext(ctx).
		Where("status IN ? AND starts_with(current_phase, ?)", IncidentStatuses, utils.PhasePodEventPrefix).
		Order("updated_at").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *incidentStore) Recover(ctx context.Context, id uuid.UUID, status, phase, newPhase string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&domain.Workload{}).
		Where("id = ? AND status = ? AND current_phase = ?", id, status, phase).
		Updates(map[string]any{"status": utils.WorkloadRunning, "current_phase": newPhase})
	return res.RowsAffected > 0, res.Error
}
