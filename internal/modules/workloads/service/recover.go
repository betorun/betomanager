package service

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
)

// Réglages par défaut de la réconciliation.
const (
	// DefaultRecoverEvery : intervalle entre deux passes.
	DefaultRecoverEvery = time.Minute
	// DefaultRecoverStableFor : durée pendant laquelle tous les pods doivent
	// tourner sans redémarrer. Un conteneur en CrashLoopBackOff est souvent
	// prêt quelques secondes entre deux plantages : sans ce délai, il
	// passerait RUNNING à tort.
	DefaultRecoverStableFor = 2 * time.Minute
	// recoverBatch : conteneurs examinés par passe.
	recoverBatch = 200
	// PhaseRecovered : phase d'un conteneur revenu à RUNNING après un incident.
	PhaseRecovered = "RECOVERED"
)

// IncidentAlerter : prévient les owners d'un passage en incident
// (alerts.Notifier).
type IncidentAlerter interface {
	WorkloadIncident(ctx context.Context, project, workload uuid.UUID, name, status string)
}

// Recoverer : remet RUNNING les conteneurs dont l'incident signalé par le
// cluster (CRASHLOOP, OOM_KILLED…) est terminé. Il surveille aussi les crons :
// une exécution échouée ne laisse aucun pod en panne à signaler (le watcher
// ne voit qu'un pod terminé), c'est donc cette passe qui pose RUN_FAILED. Le watcher ne signale que les
// pannes, jamais le retour à la normale ; sans cette passe, le statut
// d'incident restait affiché pour toujours.
type Recoverer struct {
	Store     repository.IncidentStore
	K8s       kubernetes.Interface
	Publisher realtime.Publisher
	Logger    *zap.SugaredLogger
	// Alerts : alertes email des crons en échec (facultatif).
	Alerts IncidentAlerter
	// StableFor : DefaultRecoverStableFor si nul.
	StableFor time.Duration
	// Now : horloge (tests) ; time.Now si nil.
	Now func() time.Time
}

// Run : une passe tout de suite, puis à chaque intervalle, jusqu'à ctx.Done().
func (r *Recoverer) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultRecoverEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warnw("workload recovery sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep : une passe ; renvoie le nombre de conteneurs remis à RUNNING.
func (r *Recoverer) Sweep(ctx context.Context) (int, error) {
	rows, err := r.Store.ListIncidents(ctx, recoverBatch)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, w := range rows {
		if ctx.Err() != nil {
			return recovered, ctx.Err()
		}
		healthy, err := r.healthy(ctx, &w)
		if err != nil {
			r.Logger.Debugw("workload health check failed", "workload", w.ID, "error", err)
			continue
		}
		if !healthy {
			continue
		}
		ok, err := r.Store.Recover(ctx, w.ID, w.Status, w.CurrentPhase, PhaseRecovered)
		if err != nil {
			r.Logger.Warnw("workload recovery not recorded", "workload", w.ID, "error", err)
			continue
		}
		if !ok {
			continue // statut changé entre-temps (déploiement, suppression)
		}
		recovered++
		r.Logger.Infow("workload recovered", "workload", w.ID, "from", w.Status, "phase", w.CurrentPhase)
		r.publish(ctx, &w, utils.WorkloadRunning, PhaseRecovered)
	}
	if _, err := r.SweepCrons(ctx); err != nil && ctx.Err() == nil {
		r.Logger.Warnw("cron watch pass failed", "error", err)
	}
	return recovered, nil
}

// SweepCrons : passe RUN_FAILED les crons dont la dernière exécution a échoué ;
// renvoie leur nombre.
func (r *Recoverer) SweepCrons(ctx context.Context) (int, error) {
	rows, err := r.Store.ListRunningCrons(ctx, recoverBatch)
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, w := range rows {
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
		outcome, reason, err := latestRun(ctx, r.K8s, &w)
		if err != nil {
			r.Logger.Debugw("cron run lookup failed", "workload", w.ID, "error", err)
			continue
		}
		if outcome != RunFailed {
			continue
		}
		phase := utils.PhasePodEventPrefix + reason
		ok, err := r.Store.MarkRunFailed(ctx, w.ID, w.CurrentPhase, phase)
		if err != nil {
			r.Logger.Warnw("cron failure not recorded", "workload", w.ID, "error", err)
			continue
		}
		if !ok {
			continue // statut changé entre-temps (arrêt, déploiement)
		}
		failed++
		r.Logger.Infow("cron run failed", "workload", w.ID, "reason", reason)
		r.publish(ctx, &w, utils.WorkloadRunFailed, phase)
		if r.Alerts != nil {
			r.Alerts.WorkloadIncident(ctx, w.ProjectID, w.ID, w.Name, utils.WorkloadRunFailed)
		}
	}
	return failed, nil
}

// healthy : déploiement à jour, toutes ses répliques prêtes, et chaque pod
// actif prêt depuis au moins StableFor sans redémarrage.
func (r *Recoverer) healthy(ctx context.Context, w *domain.Workload) (bool, error) {
	if isCron(w) {
		if w.CurrentPhase == utils.PhaseHelmError {
			return false, nil // le CronJob n'a peut-être jamais été posé : un redéploiement tranche
		}
		// Cron : guéri quand sa dernière exécution (planifiée ou manuelle) a réussi.
		outcome, _, err := latestRun(ctx, r.K8s, w)
		return outcome == RunSucceeded, err
	}
	dep, err := r.K8s.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Déploiement en échec : ne le lever que si le Deployment porte la version
	// voulue (image et révision des variables). Un échec avant l'application
	// laisse l'ancienne version, saine, qui ne prouve rien.
	if w.CurrentPhase == utils.PhaseHelmError && !deploymentIsCurrent(dep, w) {
		return false, nil
	}
	desired := desiredReplicas(dep)
	if desired == 0 || dep.Status.ObservedGeneration < dep.Generation ||
		dep.Status.UpdatedReplicas < desired || dep.Status.ReadyReplicas < desired ||
		dep.Status.UnavailableReplicas > 0 {
		return false, nil
	}
	pods, err := r.K8s.CoreV1().Pods(w.Namespace).List(ctx, metav1.ListOptions{LabelSelector: utils.LabelApp + "=" + w.Name})
	if err != nil {
		return false, err
	}
	cutoff := r.now().Add(-r.stableFor())
	stable := int32(0)
	for i := range pods.Items {
		p := &pods.Items[i]
		// Pods terminés (un pod expulsé reste listé en Failed) ou en cours
		// d'arrêt : hors du compte.
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if !podStableSince(p, cutoff) {
			return false, nil
		}
		stable++
	}
	return stable >= desired, nil
}

// deploymentIsCurrent : le Deployment porte l'image du conteneur et la
// révision courante de ses variables.
func deploymentIsCurrent(dep *appsv1.Deployment, w *domain.Workload) bool {
	tpl := dep.Spec.Template
	if len(tpl.Spec.Containers) == 0 || w.Image == "" || tpl.Spec.Containers[0].Image != w.Image {
		return false
	}
	if w.EnvRevision > 0 && tpl.Annotations[utils.AnnotationEnvChecksum] != strconv.FormatInt(w.EnvRevision, 10) {
		return false
	}
	return true
}

func desiredReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas == nil {
		return 1
	}
	return *dep.Spec.Replicas
}

// podStableSince : pod prêt, et chaque conteneur démarré avant cutoff.
func podStableSince(p *corev1.Pod, cutoff time.Time) bool {
	if p.Status.Phase != corev1.PodRunning || len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = !c.LastTransitionTime.After(cutoff)
		}
	}
	if !ready {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if !c.Ready || c.State.Running == nil || c.State.Running.StartedAt.After(cutoff) {
			return false
		}
	}
	return true
}

func (r *Recoverer) publish(ctx context.Context, w *domain.Workload, status, phase string) {
	if r.Publisher == nil {
		return
	}
	// Même forme que les événements de déploiement (activities.DeploymentEvent).
	event := map[string]any{
		"workload_id": w.ID.String(),
		"project_id":  w.ProjectID.String(),
		"status":      status,
		"phase":       phase,
		"at":          r.now().UTC(),
	}
	for _, ch := range []string{realtime.DeploymentChannel(w.ProjectID.String(), w.ID.String()), realtime.ProjectChannel(w.ProjectID.String())} {
		if err := r.Publisher.Publish(ctx, ch, event); err != nil {
			r.Logger.Debugw("recovery publish failed", "channel", ch, "error", err)
		}
	}
}

func (r *Recoverer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recoverer) stableFor() time.Duration {
	if r.StableFor > 0 {
		return r.StableFor
	}
	return DefaultRecoverStableFor
}
