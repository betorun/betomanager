package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idempotency "github.com/thekrauss/kubemanager/internal/infrastructure/idempotence"
	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	domain "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/vpcgateway"
)

// ProjectK8sActivities : activities Temporal manipulant des ressources Kubernetes.
// Le champ Idempotency (optionnel) déduplique les retries via ExecuteActivity.
// Kube : clients du cluster de chaque projet (les signatures des activités ne
// changent pas : workflows en cours ; le namespace suffit à le retrouver).
type ProjectK8sActivities struct {
	Kube        k8sprovider.Resolver
	Logger      *zap.SugaredLogger
	Rbac        authSvc.IRBACService
	Idempotency idempotency.AtomicManager
}

// forProject : cluster du projet (namespace en repli si l'identifiant est
// mal formé).
func (a *ProjectK8sActivities) forProject(ctx context.Context, projectID, nsName string) (*k8sprovider.ProviderK8s, error) {
	if id, err := uuid.Parse(projectID); err == nil {
		return a.Kube.ForProject(ctx, id)
	}
	return a.Kube.ForNamespace(ctx, nsName)
}

// CreateNamespace est idempotente : si le namespace existe déjà (AlreadyExists),
// on considère l'opération réussie (classique pour une activity Temporal retriable).
func (a *ProjectK8sActivities) CreateNamespace(ctx context.Context, projectID, projectName, nsName string) error {
	payload := struct {
		ProjectID, ProjectName, Namespace string
	}{projectID, projectName, nsName}

	_, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, payload,
		idempotency.ActivityOptions{ProcessingTTL: k8sProcessingTTL},
		func() (struct{}, error) {
			kube, err := a.forProject(ctx, projectID, nsName)
			if err != nil {
				return struct{}{}, fmt.Errorf("project cluster: %w", err)
			}
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
					Labels: map[string]string{
						utils.LabelManaged:   utils.LabelManagedValue,
						utils.LabelProjectID: projectID,
						// Pas le nom : libre (espaces, accents), refusé dans
						// un label, et il change au renommage.
					},
					Annotations: map[string]string{
						utils.LabelCreatedAt: time.Now().Format(time.RFC3339),
					},
				},
			}
			_, err = kube.Client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
			if err != nil && !kerrors.IsAlreadyExists(err) {
				return struct{}{}, fmt.Errorf("k8s create namespace: %w", err)
			}
			return struct{}{}, nil
		},
	)
	return err
}

func (a *ProjectK8sActivities) DeleteNamespace(ctx context.Context, nsName string) error {
	payload := struct{ Namespace string }{nsName}

	_, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, payload,
		idempotency.ActivityOptions{ProcessingTTL: k8sProcessingTTL},
		func() (struct{}, error) {
			a.Logger.Infow("deleting Kubernetes namespace", "namespace", nsName)
			kube, err := a.Kube.ForNamespace(ctx, nsName)
			if err != nil {
				return struct{}{}, fmt.Errorf("project cluster: %w", err)
			}
			err = kube.Client.CoreV1().Namespaces().Delete(ctx, nsName, metav1.DeleteOptions{})
			if err != nil && !kerrors.IsNotFound(err) {
				return struct{}{}, fmt.Errorf("failed to delete namespace %s: %w", nsName, err)
			}
			return struct{}{}, nil
		},
	)
	return err
}

func (a *ProjectK8sActivities) ReconcileProjectResources(ctx context.Context, nsName, cpu, mem string) error {
	// Spec : forme du quota ; la changer rejoue l'activité pour des
	// limites inchangées (clé d'idempotence).
	payload := struct {
		NS, CPU, Mem string
		Spec         int
	}{nsName, cpu, mem, quotaSpecVersion}

	_, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, payload,
		idempotency.ActivityOptions{ProcessingTTL: k8sProcessingTTL},
		func() (struct{}, error) {
			kube, err := a.Kube.ForNamespace(ctx, nsName)
			if err != nil {
				return struct{}{}, fmt.Errorf("project cluster: %w", err)
			}
			if _, err := kube.Client.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{}); err != nil {
				return struct{}{}, fmt.Errorf("namespace %s missing, reconciliation failed: %w", nsName, err)
			}

			cpuQ, err := resource.ParseQuantity(cpu)
			if err != nil {
				return struct{}{}, fmt.Errorf("invalid cpu limit %q: %w", cpu, err)
			}
			memQ, err := resource.ParseQuantity(mem)
			if err != nil {
				return struct{}{}, fmt.Errorf("invalid memory limit %q: %w", mem, err)
			}
			// La limite du projet est appliquée par l'API, seul chemin pour
			// créer des conteneurs. Le quota Kubernetes est un garde-fou avec
			// de la marge : à la limite exacte, le pod supplémentaire d'une
			// mise à jour progressive était refusé et le déploiement restait
			// bloqué, sans erreur, pour tout projet proche de son quota.
			cpuQ, memQ = withRollingMargin(cpuQ), withRollingMargin(memQ)
			// Composants de la plateforme dans l'espace du projet (passerelle
			// du réseau privé) : en plus, jamais pris sur la limite du client.
			cpuQ, memQ = withPlatformReserve(cpuQ, memQ)
			quotaName := projectQuotaName
			newQuotas := &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: nsName},
				Spec: corev1.ResourceQuotaSpec{
					Hard: corev1.ResourceList{
						corev1.ResourceRequestsCPU:    cpuQ,
						corev1.ResourceRequestsMemory: memQ,
						corev1.ResourceLimitsCPU:      cpuQ,
						corev1.ResourceLimitsMemory:   memQ,
					},
				},
			}

			_, err = kube.Client.CoreV1().ResourceQuotas(nsName).Get(ctx, quotaName, metav1.GetOptions{})
			if kerrors.IsNotFound(err) {
				if _, cErr := kube.Client.CoreV1().ResourceQuotas(nsName).Create(ctx, newQuotas, metav1.CreateOptions{}); cErr != nil {
					return struct{}{}, fmt.Errorf("create quota: %w", cErr)
				}
				return struct{}{}, nil
			}
			if err != nil {
				return struct{}{}, fmt.Errorf("get quota: %w", err)
			}
			if _, uErr := kube.Client.CoreV1().ResourceQuotas(nsName).Update(ctx, newQuotas, metav1.UpdateOptions{}); uErr != nil {
				return struct{}{}, fmt.Errorf("update quota: %w", uErr)
			}
			return struct{}{}, nil
		},
	)
	return err
}

// AssignDefaultOwner : conservée pour les workflows déjà en cours (le nom de
// l'activité fait partie de leur historique Temporal). Le rôle owner est
// désormais accordé dans la même transaction que la création du projet
// (repository.CreateProject), il n'y a plus rien à faire ici.
func (a *ProjectK8sActivities) AssignDefaultOwner(ctx context.Context, projectID, userID string) error {
	a.Logger.Debugw("owner already granted at project creation", "projectID", projectID, "userID", userID)
	return nil
}

func (a *ProjectK8sActivities) GetNamespaceMetrics(ctx context.Context, nsName string) (*domain.NamespaceMetrics, error) {
	kube, err := a.Kube.ForNamespace(ctx, nsName)
	if err != nil {
		return nil, fmt.Errorf("project cluster: %w", err)
	}
	podMetricsList, err := kube.MetricsClient.MetricsV1beta1().PodMetricses(nsName).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics for ns %s: %w", nsName, err)
	}

	var totalCPU, totalMem int64
	for _, pod := range podMetricsList.Items {
		for _, container := range pod.Containers {
			totalCPU += container.Usage.Cpu().MilliValue()
			totalMem += container.Usage.Memory().Value()
		}
	}

	return &domain.NamespaceMetrics{
		CPUUsage:    fmt.Sprintf("%dm", totalCPU),
		MemoryUsage: fmt.Sprintf("%dMi", totalMem/utils.BytesPerMi),
	}, nil
}

// withPlatformReserve : quota augmenté des ressources que la plateforme pose
// dans l'espace du projet (passerelle du réseau privé, ADR 0019).
func withPlatformReserve(cpu, mem resource.Quantity) (resource.Quantity, resource.Quantity) {
	cpu.Add(resource.MustParse(vpcgateway.LimitCPU))
	mem.Add(resource.MustParse(vpcgateway.LimitMemory))
	return cpu, mem
}

// withRollingMargin : quota Kubernetes pour une limite de projet.
func withRollingMargin(q resource.Quantity) resource.Quantity {
	return *resource.NewMilliQuantity(q.MilliValue()*quotaRollingMargin, q.Format)
}
