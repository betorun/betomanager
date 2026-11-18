// cron.go — cycle de vie d'un conteneur « cron » (CronJob Kubernetes).
//
// Un cron n'a pas de Deployment : arrêter / reprendre = suspendre le CronJob,
// redémarrer = lancer une exécution tout de suite (Job créé depuis le
// gabarit du CronJob, comme `kubectl create job --from=cronjob/…`).
package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// manualRunAnnotation : posée par kubectl sur un Job lancé à la main.
const manualRunAnnotation = "cronjob.kubernetes.io/instantiate"

func isCron(w *domain.Workload) bool { return kindOf(w) == domain.KindCron }

// suspendCron : suspend (true) ou reprend (false) les exécutions planifiées.
// Une exécution en cours n'est pas interrompue.
func suspendCron(ctx context.Context, k kubernetes.Interface, w *domain.Workload, suspend bool) error {
	patch := fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend)
	_, err := k.BatchV1().CronJobs(w.Namespace).Patch(ctx, w.Name, patchTypeMergePatch, []byte(patch), metav1.PatchOptions{})
	return err
}

// runCronNow : une exécution immédiate, hors horaire. Le Job appartient au
// CronJob (supprimé avec lui, compté dans son historique).
func runCronNow(ctx context.Context, k kubernetes.Interface, w *domain.Workload, now time.Time) (*batchv1.Job, error) {
	cj, err := k.BatchV1().CronJobs(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	tpl := cj.Spec.JobTemplate
	annotations := map[string]string{manualRunAnnotation: "manual"}
	for key, v := range tpl.Annotations {
		annotations[key] = v
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        manualJobName(w.Name, now),
			Namespace:   w.Namespace,
			Labels:      tpl.Labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID,
				Controller: ptr(true), BlockOwnerDeletion: ptr(true),
			}},
		},
		Spec: *tpl.Spec.DeepCopy(),
	}
	return k.BatchV1().Jobs(w.Namespace).Create(ctx, job, metav1.CreateOptions{})
}

// manualJobName : <nom>-m<horodatage>, borné à 63 caractères.
func manualJobName(name string, now time.Time) string {
	suffix := "-m" + strconv.FormatInt(now.Unix(), 36)
	if max := 63 - len(suffix); len(name) > max {
		name = name[:max]
	}
	return name + suffix
}

// cronStatus : état du CronJob ; nil s'il n'existe pas (encore).
func cronStatus(ctx context.Context, k kubernetes.Interface, w *domain.Workload) (*domain.CronStatus, *batchv1.CronJob, error) {
	cj, err := k.BatchV1().CronJobs(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	st := &domain.CronStatus{
		Schedule:  cj.Spec.Schedule,
		Suspended: cj.Spec.Suspend != nil && *cj.Spec.Suspend,
		Running:   len(cj.Status.Active),
	}
	if cj.Spec.TimeZone != nil {
		st.TimeZone = *cj.Spec.TimeZone
	}
	if t := cj.Status.LastScheduleTime; t != nil {
		v := t.Time
		st.LastScheduleTime = &v
	}
	if t := cj.Status.LastSuccessfulTime; t != nil {
		v := t.Time
		st.LastSuccessfulTime = &v
	}
	return st, cj, nil
}

// RunOutcome : issue de la dernière exécution d'un cron.
type RunOutcome int

const (
	RunNone      RunOutcome = iota // aucune exécution connue
	RunActive                      // en cours
	RunSucceeded                   // terminée avec succès
	RunFailed                      // échouée (code de sortie, durée, mémoire…)
)

// latestRun : dernier Job du cron (planifié ou lancé à la main), et la raison
// d'un échec (BackoffLimitExceeded, DeadlineExceeded…). L'historique des Jobs
// est court (3 réussis, 3 échoués) : la liste reste petite.
func latestRun(ctx context.Context, k kubernetes.Interface, w *domain.Workload) (RunOutcome, string, error) {
	jobs, err := k.BatchV1().Jobs(w.Namespace).List(ctx, metav1.ListOptions{LabelSelector: utils.LabelApp + "=" + w.Name})
	if err != nil {
		return RunNone, "", err
	}
	var last *batchv1.Job
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if last == nil || last.CreationTimestamp.Before(&j.CreationTimestamp) ||
			(last.CreationTimestamp.Equal(&j.CreationTimestamp) && j.Name > last.Name) {
			last = j
		}
	}
	if last == nil {
		return RunNone, "", nil
	}
	// Exécution d'une version précédente (image remplacée depuis) : elle ne
	// dit rien de la version en service.
	if c := last.Spec.Template.Spec.Containers; len(c) > 0 && w.Image != "" && c[0].Image != w.Image {
		return RunNone, "", nil
	}
	for _, c := range last.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			reason := c.Reason
			if reason == "" {
				reason = "JobFailed"
			}
			return RunFailed, reason, nil
		case batchv1.JobComplete:
			return RunSucceeded, "", nil
		}
	}
	return RunActive, "", nil
}

// cronRevision : révision des variables portée par le gabarit du CronJob ;
// found=false s'il n'existe pas.
func cronRevision(ctx context.Context, k kubernetes.Interface, w *domain.Workload) (rev string, found bool, err error) {
	cj, err := k.BatchV1().CronJobs(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return cj.Spec.JobTemplate.Spec.Template.Annotations[utils.AnnotationEnvChecksum], true, nil
}

// pickLogPod : web / worker : le premier pod Running ; cron : la dernière
// exécution (pod le plus récent), terminée ou non.
func pickLogPod(pods []corev1.Pod, cron bool) corev1.Pod {
	if cron {
		sorted := append([]corev1.Pod(nil), pods...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[j].CreationTimestamp.Before(&sorted[i].CreationTimestamp)
		})
		return sorted[0]
	}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			return p
		}
	}
	return pods[0]
}

func ptr[T any](v T) *T { return &v }
