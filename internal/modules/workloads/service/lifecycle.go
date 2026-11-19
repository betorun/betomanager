// lifecycle.go — opérations de cycle de vie des workloads.
//
// Division claire :
//   - Deploy / Delete passent par Temporal (atomicité + retries sur ops longues)
//   - Stop / Start / Restart / Rollback : appels K8s/Helm directs (ops atomiques)
//   - Get / List / LiveStatus / Logs / Events : read-only, pas de workflow
package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/google/uuid"
	"helm.sh/helm/v3/pkg/action"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/thekrauss/kubemanager/internal/infrastructure/helm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

//  CRUD read-side

func (s *WorkloadService) Get(ctx context.Context, id uuid.UUID) (*domain.Workload, error) {
	return s.Repo.GetByID(ctx, id)
}

func (s *WorkloadService) List(ctx context.Context, projectID uuid.UUID) ([]domain.Workload, error) {
	return s.Repo.ListByProject(ctx, projectID)
}

//  Delete : passe par Temporal (Saga : Helm uninstall + DB soft-delete)

func (s *WorkloadService) Delete(ctx context.Context, id uuid.UUID) error {
	workload, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return &betoerrors.Error{Code: betoerrors.CodeNotFound, Message: "workload not found", Cause: err}
	}

	// Suppression déjà lancée : même résultat, rien à refaire. Sinon (workflow
	// jamais démarré, DELETING resté en base), elle reprend ici.
	deleteID := workflows.WorkflowIDDeletePrefix + id.String()
	running, err := s.Distributor.IsRunning(ctx, deleteID)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Marque immédiatement DELETING : plus aucun déploiement ne peut démarrer
	// (écriture conditionnelle, y compris dans le workflow).
	if err := s.Repo.UpdateStatus(ctx, id, utils.WorkloadDeleting, utils.PhaseHelmUninstalling); err != nil {
		return err
	}

	// DELETING est écrit : la suite ne doit pas dépendre de la requête.
	ctx = context.WithoutCancel(ctx)
	s.stopDeploys(ctx, id)

	_, err = s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: deleteID},
		workflows.DeleteWorkloadWorkflowName,
		DeleteWorkloadInput{
			WorkloadID:  id.String(),
			ProjectID:   workload.ProjectID.String(),
			Namespace:   workload.Namespace,
			ReleaseName: workload.Name,
		},
	)
	if err == nil || temporal.IsAlreadyStarted(err) {
		return nil
	}
	s.compensate(ctx, "workload delete failure not recorded", id, func(ctx context.Context) error {
		return s.Repo.UpdateStatus(ctx, id, utils.WorkloadDeleteFailed, utils.PhaseHelmUninstallErr)
	})
	return err
}

// stopDeploys : un déploiement encore en cours réinstallerait le conteneur après
// sa désinstallation : il resterait dans Kubernetes, hors de la base, et
// consommerait le quota du projet. Annulation plutôt qu'arrêt brutal : l'activité
// Helm reçoit l'annulation et s'interrompt, et le workflow ne se ferme qu'ensuite
// (WaitForCancellation). L'arrêt brutal ne sert que si rien ne répond (worker
// tombé) : le prochain heartbeat d'une activité encore vivante l'interrompra.
func (s *WorkloadService) stopDeploys(ctx context.Context, id uuid.UUID) {
	for _, prefix := range []string{workflows.WorkflowIDDeployPrefix, workflows.WorkflowIDUpdatePrefix} {
		wfID := prefix + id.String()
		running, err := s.Distributor.IsRunning(ctx, wfID)
		if err != nil {
			s.warn("deploy state unavailable before delete", "workflow", wfID, "error", err)
		} else if !running {
			continue
		}
		if err := s.Distributor.CancelWorkflow(ctx, wfID, ""); err != nil {
			s.warn("deploy not cancelled before delete", "workflow", wfID, "error", err)
		}
		if s.waitStopped(ctx, wfID) {
			continue
		}
		s.warn("deploy still running after cancel, terminating", "workflow", wfID)
		if err := s.Distributor.TerminateWorkflow(ctx, wfID, "", "workload deleted"); err != nil {
			s.warn("deploy not terminated before delete", "workflow", wfID, "error", err)
		}
	}
}

// waitStopped : attend la fin du workflow, dans la limite de deployStopWait.
func (s *WorkloadService) waitStopped(ctx context.Context, workflowID string) bool {
	deadline := time.After(deployStopWait)
	ticker := time.NewTicker(deployStopPoll)
	defer ticker.Stop()
	for {
		if running, err := s.Distributor.IsRunning(ctx, workflowID); err == nil && !running {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-ticker.C:
		}
	}
}

// DeleteWorkloadInput : signature partagée entre service et workflow.
// Duplique le type de workflows/ pour éviter un import cycle.
type DeleteWorkloadInput struct {
	WorkloadID  string
	ProjectID   string
	Namespace   string
	ReleaseName string
}

//  Lifecycle K8s direct (Stop/Start/Restart)

// Stop : scale deployment à 0. Le workload reste en DB (billing on-demand).
// Idempotent : scale 0 → 0 est un no-op côté K8s.
func (s *WorkloadService) Stop(ctx context.Context, id uuid.UUID) error {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureNotDeleting(w); err != nil {
		return err
	}
	if err := ensureBuilt(w); err != nil {
		return err
	}
	if isCron(w) {
		if err := suspendCron(ctx, s.K8sClient, w, true); err != nil {
			return fmt.Errorf("suspend cronjob: %w", err)
		}
		return s.Repo.UpdateStatus(ctx, id, utils.WorkloadStopped, utils.PhaseCronSuspended)
	}
	if err := s.scaleTo(ctx, w, 0); err != nil {
		return fmt.Errorf("scale to 0: %w", err)
	}
	return s.Repo.UpdateStatus(ctx, id, utils.WorkloadStopped, utils.PhaseScaledToZero)
}

// Start : remonte le deployment à `w.Replicas` (valeur DB, pas 1 en dur).
func (s *WorkloadService) Start(ctx context.Context, id uuid.UUID) error {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.canRun(ctx, w.ProjectID); err != nil {
		return err
	}
	if err := ensureNotDeleting(w); err != nil {
		return err
	}
	if err := ensureBuilt(w); err != nil {
		return err
	}
	if isCron(w) {
		if err := suspendCron(ctx, s.K8sClient, w, false); err != nil {
			return fmt.Errorf("resume cronjob: %w", err)
		}
		return s.Repo.UpdateStatus(ctx, id, utils.WorkloadRunning, utils.PhaseCronResumed)
	}
	target := int32(w.Replicas)
	if target <= 0 {
		target = 1
	}
	if err := s.scaleTo(ctx, w, target); err != nil {
		return fmt.Errorf("scale to %d: %w", target, err)
	}
	return s.Repo.UpdateStatus(ctx, id, utils.WorkloadRunning, utils.PhaseStarted)
}

// Restart : rollout restart via patch de l'annotation.
// Pattern standard `kubectl rollout restart deployment`.
func (s *WorkloadService) Restart(ctx context.Context, id uuid.UUID) error {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureNotDeleting(w); err != nil {
		return err
	}
	if err := ensureBuilt(w); err != nil {
		return err
	}
	if isCron(w) {
		// Cron : « redémarrer » = exécuter tout de suite. Le statut ne change
		// pas (un cron suspendu reste suspendu).
		if _, err := runCronNow(ctx, s.K8sClient, w, time.Now()); err != nil {
			return fmt.Errorf("run cronjob now: %w", err)
		}
		return nil
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		utils.AnnotationRestartedAt, time.Now().UTC().Format(time.RFC3339),
	)
	_, err = s.K8sClient.AppsV1().Deployments(w.Namespace).Patch(
		ctx, w.Name,
		patchTypeMergePatch, []byte(patch),
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch deployment: %w", err)
	}
	// Redémarrage progressif : les anciens pods servent jusqu'à ce que les
	// nouveaux soient prêts, le conteneur reste RUNNING (avant : RESTARTING,
	// que rien ne faisait jamais repasser à RUNNING).
	return s.Repo.UpdateStatus(ctx, id, utils.WorkloadRunning, utils.PhaseRolloutRestart)
}

// Rollback : helm rollback à la révision cible (0 = revision précédente).
// Utilise le Helm SDK avec le REST config partagé par l'app.
func (s *WorkloadService) Rollback(ctx context.Context, id uuid.UUID, toRevision int) error {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.canRun(ctx, w.ProjectID); err != nil {
		return err
	}
	if err := ensureNotDeleting(w); err != nil {
		return err
	}
	if err := ensureBuilt(w); err != nil {
		return err
	}
	cfg, err := s.newHelmActionConfig(w.Namespace)
	if err != nil {
		return fmt.Errorf("helm config: %w", err)
	}

	rb := action.NewRollback(cfg)
	rb.Version = toRevision // 0 = previous release
	rb.Wait = true
	rb.Timeout = helmRollbackTimeout

	if err := rb.Run(w.Name); err != nil {
		return fmt.Errorf("helm rollback: %w", err)
	}
	return s.Repo.UpdateStatus(ctx, id, utils.WorkloadRunning, utils.PhaseRolledBack)
}

//  Observabilité

func (s *WorkloadService) LiveStatus(ctx context.Context, id uuid.UUID) (*domain.LiveStatus, error) {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	status := &domain.LiveStatus{
		ID:          w.ID.String(),
		Name:        w.Name,
		Namespace:   w.Namespace,
		Status:      w.Status,
		Phase:       w.CurrentPhase,
		ExternalURL: w.ExternalURL,
		InternalURL: InternalURL(w),
		Image:       w.Image,
		Replicas:    int32(w.Replicas),
		UpdatedAt:   w.UpdatedAt,
		Kind:        kindOf(w),
		ManagedBy:   w.ManagedBy,
	}
	if spec, err := s.specFor(w); err == nil {
		status.Command, status.Args = spec.Command, spec.Args
	}
	if w.PersistenceEnabled {
		status.Disk = &domain.DiskStatus{Size: w.StorageSize, BackedUp: false}
	}

	// Best-effort : si le deployment n'existe pas encore (en cours de déploiement)
	// on retourne juste l'état DB sans la partie K8s.
	if isCron(w) {
		cron, _, err := cronStatus(ctx, s.K8sClient, w)
		if err != nil {
			s.Logger.Warnw("cronjob lookup failed", "workload", id, "error", err)
		}
		status.Cron = cron
		if cron != nil {
			status.ReadyReplicas = int32(cron.Running)
		}
	} else if dep, err := s.K8sClient.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{}); err == nil {
		status.ReadyReplicas = dep.Status.ReadyReplicas
		status.AvailableReplicas = dep.Status.AvailableReplicas
		s.recoverOnRead(ctx, w, dep, status)
	} else if !kerrors.IsNotFound(err) {
		s.Logger.Warnw("deployment lookup failed", "workload", id, "error", err)
	}

	// Liste des pods (labelSelector standard Helm).
	pods, err := s.K8sClient.CoreV1().Pods(w.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: utils.LabelApp + "=" + w.Name,
	})
	if err == nil {
		status.Pods = make([]domain.PodStatus, 0, len(pods.Items))
		for _, p := range pods.Items {
			ps := domain.PodStatus{
				Name:     p.Name,
				Phase:    string(p.Status.Phase),
				NodeName: p.Spec.NodeName,
			}
			if p.Status.StartTime != nil {
				ps.StartedAt = p.Status.StartTime.Time
			}
			for _, c := range p.Status.ContainerStatuses {
				ps.RestartCount += c.RestartCount
				ps.Ready = ps.Ready || c.Ready
			}
			status.Pods = append(status.Pods, ps)
			if status.PullFailure == nil {
				status.PullFailure = s.pullFailure(ctx, &p)
			}
		}
	}

	return status, nil
}

// pullFailure : cause d'un conteneur bloqué sur son image (ADR 0011). Le
// message d'ImagePullBackOff est vague : le dernier événement « Failed » du
// pod porte celui du registre.
func (s *WorkloadService) pullFailure(ctx context.Context, p *corev1.Pod) *domain.PullFailure {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, c := range statuses {
		w := c.State.Waiting
		if w == nil || !domain.IsPullWaiting(w.Reason) {
			continue
		}
		image := c.Image
		for _, spec := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
			if spec.Name == c.Name {
				image = spec.Image // c.Image peut être vide tant que rien n'est tiré
			}
		}
		f := domain.ClassifyPull(image, w.Reason, w.Message)
		if f.Cause == domain.PullUnknown {
			if msg := s.lastPullEvent(ctx, p); msg != "" {
				f = domain.ClassifyPull(image, domain.ReasonErrImagePull, msg)
			}
		}
		return &f
	}
	return nil
}

// lastPullEvent : message du dernier échec de récupération d'image du pod.
func (s *WorkloadService) lastPullEvent(ctx context.Context, p *corev1.Pod) string {
	events, err := s.K8sClient.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + p.Name + ",reason=Failed",
	})
	if err != nil {
		return ""
	}
	var last *corev1.Event
	for i := range events.Items {
		e := &events.Items[i]
		if !strings.Contains(e.Message, "pull") && !strings.Contains(e.Message, "image") {
			continue
		}
		if last == nil || eventTime(e).After(eventTime(last)) {
			last = e
		}
	}
	if last == nil {
		return ""
	}
	return last.Message
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// Logs : stream les logs du premier pod en état Running (ou Pending si aucun Ready).
// Le ReadCloser retourné DOIT être fermé par l'appelant.
func (s *WorkloadService) Logs(ctx context.Context, id uuid.UUID, opts LogOptions) (io.ReadCloser, error) {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	pods, err := s.K8sClient.CoreV1().Pods(w.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: utils.LabelApp + "=" + w.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "no pods found for workload")
	}

	target := pickLogPod(pods.Items, isCron(w))

	podOpts := &corev1.PodLogOptions{
		Follow:     opts.Follow,
		Previous:   opts.Previous,
		Timestamps: opts.Timestamps,
	}
	if opts.TailLines > 0 {
		podOpts.TailLines = &opts.TailLines
	}
	if opts.SinceSeconds > 0 {
		podOpts.SinceSeconds = &opts.SinceSeconds
	}

	req := s.K8sClient.CoreV1().Pods(w.Namespace).GetLogs(target.Name, podOpts)
	return req.Stream(ctx)
}

// Events : K8s events récents filtrés par label workload.
// L'ordre est chronologique descendant (plus récent en premier).
func (s *WorkloadService) Events(ctx context.Context, id uuid.UUID) ([]domain.WorkloadEvent, error) {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// K8s Events ne supportent pas labelSelector → on filtre par fieldSelector
	// sur le sub-resource "involvedObject.namespace" puis post-filter par préfixe
	// du nom (ressources créées par Helm ont le release name en préfixe).
	list, err := s.K8sClient.CoreV1().Events(w.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	out := make([]domain.WorkloadEvent, 0, len(list.Items))
	for _, e := range list.Items {
		// Filtre : objets dont le nom commence par le release name.
		if e.InvolvedObject.Name != w.Name && len(e.InvolvedObject.Name) > len(w.Name) {
			prefix := e.InvolvedObject.Name[:len(w.Name)]
			if prefix != w.Name {
				continue
			}
		} else if e.InvolvedObject.Name != w.Name {
			continue
		}
		ev := domain.WorkloadEvent{
			Type:    e.Type,
			Reason:  e.Reason,
			Message: e.Message,
			Object:  fmt.Sprintf("%s/%s", e.InvolvedObject.Kind, e.InvolvedObject.Name),
			Count:   e.Count,
		}
		ev.FirstSeen, ev.LastSeen = eventTimes(e)
		out = append(out, ev)
	}
	return out, nil
}

// Metrics : consommation actuelle CPU/RAM mesurée par le Metrics Server
// pour tous les pods portant le label `km-workload-id=<uuid>`, augmentée des
// valeurs réservées stockées en DB (Replicas × CPULimit/MemoryLimit).
//
// Metrics Server requis dans le cluster cible. Si absent, renvoie uniquement
// les valeurs réservées et CPUUsage/MemoryUsage = "0m"/"0Mi".
func (s *WorkloadService) Metrics(ctx context.Context, id uuid.UUID) (*domain.WorkloadMetrics, error) {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	out := &domain.WorkloadMetrics{
		WorkloadID:     id.String(),
		CPUUsage:       zeroCPUUsage,
		MemoryUsage:    zeroMemoryUsage,
		ReservedCPU:    w.CPULimit,
		ReservedMemory: w.MemoryLimit,
		Replicas:       w.Replicas,
	}

	mClient, err := metricsv1.NewForConfig(s.K8sConfig)
	if err != nil {
		s.Logger.Warnw("metrics client unavailable, returning reserved only", "error", err)
		return out, nil
	}

	selector := utils.LabelKmWorkloadID + "=" + id.String()
	podMetrics, err := mClient.MetricsV1beta1().PodMetricses(w.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		s.Logger.Warnw("metrics server lookup failed", "workload", id, "error", err)
		return out, nil
	}

	var totalCPUMilli, totalMemBytes int64
	for _, pod := range podMetrics.Items {
		for _, c := range pod.Containers {
			totalCPUMilli += c.Usage.Cpu().MilliValue()
			totalMemBytes += c.Usage.Memory().Value()
		}
	}
	out.PodCount = len(podMetrics.Items)
	out.CPUUsage = fmt.Sprintf("%dm", totalCPUMilli)
	out.MemoryUsage = fmt.Sprintf("%dMi", totalMemBytes/utils.BytesPerMi)
	return out, nil
}

//  Helpers privés

func (s *WorkloadService) scaleTo(ctx context.Context, w *domain.Workload, replicas int32) error {
	scale, err := s.K8sClient.AppsV1().Deployments(w.Namespace).GetScale(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if scale.Spec.Replicas == replicas {
		return nil
	}
	scale.Spec.Replicas = replicas
	_, err = s.K8sClient.AppsV1().Deployments(w.Namespace).UpdateScale(ctx, w.Name, scale, metav1.UpdateOptions{})
	return err
}

func (s *WorkloadService) newHelmActionConfig(namespace string) (*action.Configuration, error) {
	return helm.NewActionConfig(s.K8sConfig, namespace, func(format string, v ...interface{}) {
		s.Logger.Debugf(format, v...)
	})
}

// eventTimes : premier et dernier passage d'un événement. Les événements de
// l'API events.k8s.io (planificateur, kubelet récents) laissent
// FirstTimestamp vide et datent dans EventTime / Series : sans repli, la
// console affichait « 1 janv. 1 ».
func eventTimes(e corev1.Event) (first, last time.Time) {
	for _, t := range []time.Time{e.FirstTimestamp.Time, e.EventTime.Time, e.CreationTimestamp.Time} {
		if !t.IsZero() {
			first = t
			break
		}
	}
	last = e.LastTimestamp.Time
	if last.IsZero() && e.Series != nil {
		last = e.Series.LastObservedTime.Time
	}
	if last.IsZero() {
		last = first
	}
	return first, last
}
