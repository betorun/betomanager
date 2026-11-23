package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

func (s *ProjectService) GetProjectStatus(ctx context.Context, projectID string) (*domain.ProjectStatusResponse, error) {

	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	res := &domain.ProjectStatusResponse{
		ProjectID: project.ID.String(),
		Status:    project.Status,
		CreatedAt: project.CreatedAt,
	}

	switch project.Status {

	case utils.ProjectStatusReady:
		nsName := project.Namespace
		ns, err := s.namespace(ctx, project.ID, nsName)

		if err != nil {
			res.Status = utils.ProjectStatusError
			res.Phase = utils.PhaseProvisioningFailed
		} else {
			res.K8sInfo = &domain.NamespaceStatus{
				Name:   ns.Name,
				Exists: true,
				Phase:  string(ns.Status.Phase),
			}
			res.Phase = utils.PhaseProvisioningSuccess
		}

	case utils.ProjectStatusProvisioning:
		res.Phase = utils.PhaseK8sNamespaceLinking

	case utils.ProjectStatusError:
		res.Phase = utils.PhaseProvisioningFailed

	case utils.ProjectStatusDeleting:
		res.Phase = utils.PhaseRollbackInitiated

	default:
		res.Phase = utils.ProjectStatusUnknown
	}

	return res, nil
}

// namespace : namespace du projet sur son cluster.
func (s *ProjectService) namespace(ctx context.Context, project uuid.UUID, name string) (*corev1.Namespace, error) {
	kube, err := s.Kube.ForProject(ctx, project)
	if err != nil {
		return nil, err
	}
	return kube.Client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
}

type ProjectOverview struct {
	ReservedStorage int64 // Ce qui est en DB (ex: 10Gi)
	CurrentUsage    int64 // Ce qui vient de K8s Metrics (ex: 2Gi)
	QuotaLimit      int64 // La limite max autorisée pour ce projet (ex: 20Gi)
}

func (s *ProjectService) GetMetrics(ctx context.Context, projectID string) (*domain.NamespaceMetrics, error) {
	project, err := s.Repos.GetProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	nsName := project.Namespace

	kube, err := s.Kube.ForProject(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("project cluster: %w", err)
	}

	podMetrics, err := kube.MetricsClient.MetricsV1beta1().PodMetricses(nsName).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("metrics server error: %w", err)
	}

	var totalCPU, totalMem int64
	for _, pod := range podMetrics.Items {
		for _, container := range pod.Containers {
			totalCPU += container.Usage.Cpu().MilliValue()
			totalMem += container.Usage.Memory().Value()
		}
	}

	// Une panne ne s'affiche pas comme une réservation nulle.
	resCPU, resMem, resStorage, err := s.WorkloadRepo.GetTotalUsageByProject(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("project usage: %w", err)
	}

	// Disques réservés : conteneurs (Mi) et bases gérées, en octets puis
	// affichés en Mi (l'arrondi au Gi entier montrait « 0 o »).
	storageBytes := resStorage * utils.BytesPerMi
	if s.Databases != nil {
		dbBytes, err := s.Databases.StorageBytes(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("project databases storage: %w", err)
		}
		storageBytes += dbBytes
	}

	out := &domain.NamespaceMetrics{
		CPUUsage:    fmt.Sprintf("%dm", totalCPU),
		MemoryUsage: fmt.Sprintf("%dMi", totalMem/utils.BytesPerMi),
		ReservedUsage: domain.UsageDTO{
			CPU:     fmt.Sprintf("%dm", resCPU),
			Memory:  fmt.Sprintf("%dMi", resMem),
			Storage: fmt.Sprintf("%dMi", storageBytes/utils.BytesPerMi),
		},
		Limits: domain.UsageDTO{CPU: project.CpuLimit, Memory: project.MemoryLimit, Storage: project.StorageLimit},
	}
	if s.Images != nil {
		used, quota, err := s.Images.Usage(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("project images usage: %w", err)
		}
		out.Images = &domain.ImagesUsage{UsedBytes: used, QuotaBytes: quota}
	}
	return out, nil
}
