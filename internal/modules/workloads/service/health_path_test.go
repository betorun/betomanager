package service

import (
	"errors"
	"testing"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// Chemin de santé : web seulement, validé, appliqué aux trois sondes.
func TestApplyProcessHealthPath(t *testing.T) {
	path := "/healthz"
	spec := &domain.CreateWorkloadRequest{HealthPath: "/", ReadinessPath: "/ready"}
	if err := applyProcess(&domain.Workload{Kind: domain.KindWeb}, spec, &domain.UpdateWorkloadRequest{HealthPath: &path}); err != nil {
		t.Fatal(err)
	}
	if spec.HealthPath != path || spec.ReadinessPath != path {
		t.Fatalf("sondes non mises à jour : %+v", spec)
	}

	bad := "healthz"
	if err := applyProcess(&domain.Workload{Kind: domain.KindWeb}, &domain.CreateWorkloadRequest{}, &domain.UpdateWorkloadRequest{HealthPath: &bad}); !errors.Is(err, domain.ErrInvalidHealthPath) {
		t.Fatalf("chemin invalide accepté : %v", err)
	}
	if err := applyProcess(&domain.Workload{Kind: domain.KindWorker}, &domain.CreateWorkloadRequest{}, &domain.UpdateWorkloadRequest{HealthPath: &path}); !errors.Is(err, domain.ErrNotWeb) {
		t.Fatalf("worker accepté : %v", err)
	}

	keep := &domain.CreateWorkloadRequest{HealthPath: "/up"}
	if err := applyProcess(&domain.Workload{Kind: domain.KindWeb}, keep, &domain.UpdateWorkloadRequest{}); err != nil || keep.HealthPath != "/up" {
		t.Fatalf("absent doit laisser inchangé : %v %+v", err, keep)
	}
}
