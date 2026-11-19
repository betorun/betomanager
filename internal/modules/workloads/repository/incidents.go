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
	utils.WorkloadRunFailed,
}

// IncidentStore : conteneurs en incident signalé par le cluster, et leur retour
// à RUNNING.
type IncidentStore interface {
	// ListIncidents : statut d'incident et phase POD_*, plus les déploiements
	// tombés en délai dépassé (FAILED, HELM_ERROR) : leurs pods peuvent finir
	// par démarrer (application lente) ; la passe vérifie alors que le
	// Deployment porte bien la version voulue avant de lever l'incident.
	ListIncidents(ctx context.Context, limit int) ([]domain.Workload, error)
	// Recover : RUNNING si le statut et la phase n'ont pas changé depuis la
	// lecture (un déploiement lancé entre-temps garde la main). false : rien
	// n'a été écrit.
	Recover(ctx context.Context, id uuid.UUID, status, phase, newPhase string) (bool, error)
	// ListRunningCrons : crons en service (ni arrêtés, ni en déploiement, ni
	// déjà en incident).
	ListRunningCrons(ctx context.Context, limit int) ([]domain.Workload, error)
	// MarkRunFailed : RUN_FAILED si le conteneur est toujours RUNNING dans la
	// phase lue. false : rien n'a été écrit.
	MarkRunFailed(ctx context.Context, id uuid.UUID, phase, newPhase string) (bool, error)
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
		Where("(status IN ? AND starts_with(current_phase, ?)) OR (status = ? AND current_phase = ?)",
			IncidentStatuses, utils.PhasePodEventPrefix, utils.WorkloadFailed, utils.PhaseHelmError).
		Order("updated_at").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *incidentStore) ListRunningCrons(ctx context.Context, limit int) ([]domain.Workload, error) {
	var rows []domain.Workload
	err := s.db.WithContext(ctx).
		Where("kind = ? AND status = ?", domain.KindCron, utils.WorkloadRunning).
		Order("updated_at").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *incidentStore) MarkRunFailed(ctx context.Context, id uuid.UUID, phase, newPhase string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&domain.Workload{}).
		Where("id = ? AND status = ? AND current_phase = ?", id, utils.WorkloadRunning, phase).
		Updates(map[string]any{"status": utils.WorkloadRunFailed, "current_phase": newPhase})
	return res.RowsAffected > 0, res.Error
}

func (s *incidentStore) Recover(ctx context.Context, id uuid.UUID, status, phase, newPhase string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&domain.Workload{}).
		Where("id = ? AND status = ? AND current_phase = ?", id, status, phase).
		Updates(map[string]any{"status": utils.WorkloadRunning, "current_phase": newPhase})
	return res.RowsAffected > 0, res.Error
}
