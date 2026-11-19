package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/workflows"
)

// ErrInvalidProjectLimits : limite illisible ou nulle.
var ErrInvalidProjectLimits = errors.New("invalid project limits: use Kubernetes quantities (e.g. 2000m, 4Gi)")

// ErrLimitsBelowUsage : la nouvelle limite est sous ce que les conteneurs du
// projet réservent déjà.
var ErrLimitsBelowUsage = errors.New("the new limit is below what the project's containers already reserve")

// ErrLimitsUpdateInProgress : une modification des limites du projet est
// encore en cours d'application.
var ErrLimitsUpdateInProgress = errors.New("a limits update is already in progress for this project")

// SetLimits : nouvelles limites d'un projet (staff de la plateforme). Un champ
// vide garde la valeur actuelle. La base est mise à jour d'abord (l'API s'y
// réfère pour accepter les déploiements), puis un workflow ajuste le quota
// Kubernetes ; le rappeler avec les mêmes valeurs reprend un ajustement échoué.
func (s *ProjectService) SetLimits(ctx context.Context, projectID string, in typesproject.ProjectLimits) (*typesproject.ProjectLimitsResponse, error) {
	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	next := typesproject.ProjectLimits{
		CPU:     pick(in.CPU, project.CpuLimit),
		Memory:  pick(in.Memory, project.MemoryLimit),
		Storage: pick(in.Storage, project.StorageLimit),
	}
	cpu, err := positiveQuantity(next.CPU)
	if err != nil {
		return nil, err
	}
	mem, err := positiveQuantity(next.Memory)
	if err != nil {
		return nil, err
	}
	if _, err := positiveQuantity(next.Storage); err != nil {
		return nil, err
	}

	// Pas sous l'existant : les conteneurs déjà là dépasseraient la limite et
	// le prochain déploiement serait refusé sans que rien n'ait changé.
	if s.WorkloadRepo != nil {
		usedCPU, usedMemMi, _, err := s.WorkloadRepo.GetTotalUsageByProject(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("project usage: %w", err)
		}
		if cpu.MilliValue() < usedCPU || mem.Value()/(1<<20) < usedMemMi {
			return nil, ErrLimitsBelowUsage
		}
	}

	if err := s.Repos.UpdateProjectLimits(ctx, projectID, next.CPU, next.Memory, next.Storage); err != nil {
		return nil, err
	}
	handle, err := s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDLimitsPrefix + projectID},
		workflows.UpdateProjectLimitsWorkflow, workflows.UpdateProjectLimitsInput{
			ProjectID:   projectID,
			Namespace:   project.Namespace,
			CpuLimit:    next.CPU,
			MemoryLimit: next.Memory,
		},
	)
	if temporal.IsAlreadyStarted(err) {
		return nil, ErrLimitsUpdateInProgress
	}
	if err != nil {
		s.Logger.Errorw("Failed to start limits workflow", "projectID", projectID, "error", err)
		return nil, err
	}
	return &typesproject.ProjectLimitsResponse{
		ProjectID:  projectID,
		Limits:     typesproject.UsageDTO{CPU: next.CPU, Memory: next.Memory, Storage: next.Storage},
		WorkflowID: handle.WorkflowID,
	}, nil
}

func pick(v, current string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return current
}

func positiveQuantity(v string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil || q.Sign() <= 0 {
		return resource.Quantity{}, ErrInvalidProjectLimits
	}
	return q, nil
}
