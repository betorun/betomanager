package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

func cronWorkload() *domain.Workload {
	return &domain.Workload{ID: uuid.New(), Name: "nightly", Namespace: "km-x", Kind: domain.KindCron, Image: "r/p:1"}
}

func cronJob(last, ok *time.Time) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "km-x", UID: "uid-1"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *", TimeZone: ptr("Africa/Dakar"),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{utils.LabelApp: "nightly"}},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{utils.AnnotationEnvChecksum: "4"}},
					Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "app", Image: "r/p:1"}}},
				}},
			},
		},
	}
	if last != nil {
		cj.Status.LastScheduleTime = &metav1.Time{Time: *last}
	}
	if ok != nil {
		cj.Status.LastSuccessfulTime = &metav1.Time{Time: *ok}
	}
	return cj
}

func TestSuspendCron_StopAndResume(t *testing.T) {
	k := fake.NewSimpleClientset(cronJob(nil, nil))
	w := cronWorkload()
	ctx := context.Background()

	require.NoError(t, suspendCron(ctx, k, w, true))
	st, _, err := cronStatus(ctx, k, w)
	require.NoError(t, err)
	assert.True(t, st.Suspended)
	assert.Equal(t, "Africa/Dakar", st.TimeZone)

	require.NoError(t, suspendCron(ctx, k, w, false))
	st, _, _ = cronStatus(ctx, k, w)
	assert.False(t, st.Suspended)
}

func TestRunCronNow_JobFromTemplateOwnedByCronJob(t *testing.T) {
	k := fake.NewSimpleClientset(cronJob(nil, nil))
	job, err := runCronNow(context.Background(), k, cronWorkload(), recoverNow)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(job.Name, "nightly-m"))
	assert.Equal(t, "manual", job.Annotations[manualRunAnnotation])
	assert.Equal(t, "nightly", job.Labels[utils.LabelApp], "les journaux retrouvent le pod par ce label")
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, "CronJob", job.OwnerReferences[0].Kind)
	assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
}

func TestRunCronNow_MissingCronJob(t *testing.T) {
	_, err := runCronNow(context.Background(), fake.NewSimpleClientset(), cronWorkload(), recoverNow)
	assert.Error(t, err)
}

func TestManualJobName_Bounded(t *testing.T) {
	name := manualJobName(strings.Repeat("a", 70), recoverNow)
	assert.LessOrEqual(t, len(name), 63)
	assert.Contains(t, name, "-m")
}

// job : exécution du cron « nightly », créée il y a `age`, avec l'image donnée.
func job(name string, age time.Duration, image string, cond batchv1.JobConditionType, reason string) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "km-x", Labels: map[string]string{utils.LabelApp: "nightly"},
			CreationTimestamp: metav1.NewTime(recoverNow.Add(-age))},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}}}},
	}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Reason: reason}}
	}
	return j
}

func TestLatestRun(t *testing.T) {
	ctx, w := context.Background(), cronWorkload()
	cases := []struct {
		name   string
		jobs   []*batchv1.Job
		want   RunOutcome
		reason string
	}{
		{"aucune exécution", nil, RunNone, ""},
		{"dernière réussie", []*batchv1.Job{job("a", 2*time.Hour, "r/p:1", batchv1.JobFailed, "BackoffLimitExceeded"), job("b", time.Hour, "r/p:1", batchv1.JobComplete, "")}, RunSucceeded, ""},
		{"dernière échouée", []*batchv1.Job{job("a", 2*time.Hour, "r/p:1", batchv1.JobComplete, ""), job("b", time.Hour, "r/p:1", batchv1.JobFailed, "DeadlineExceeded")}, RunFailed, "DeadlineExceeded"},
		{"en cours", []*batchv1.Job{job("a", time.Minute, "r/p:1", "", "")}, RunActive, ""},
		{"ancienne image ignorée", []*batchv1.Job{job("a", time.Hour, "r/p:0", batchv1.JobFailed, "BackoffLimitExceeded")}, RunNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := fake.NewSimpleClientset()
			for _, j := range tc.jobs {
				_, err := k.BatchV1().Jobs("km-x").Create(ctx, j, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			got, reason, err := latestRun(ctx, k, w)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.reason, reason)
		})
	}
}

func TestCronRevision(t *testing.T) {
	k := fake.NewSimpleClientset(cronJob(nil, nil))
	rev, found, err := cronRevision(context.Background(), k, cronWorkload())
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "4", rev)

	_, found, err = cronRevision(context.Background(), fake.NewSimpleClientset(), cronWorkload())
	require.NoError(t, err)
	assert.False(t, found)
}

func TestPickLogPod(t *testing.T) {
	older := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(recoverNow.Add(-time.Hour))},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	newer := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", CreationTimestamp: metav1.NewTime(recoverNow)},
		Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	pods := []corev1.Pod{older, newer}
	assert.Equal(t, "new", pickLogPod(pods, true).Name, "cron : la dernière exécution, même échouée")
	assert.Equal(t, "old", pickLogPod(pods, false).Name, "web : le pod qui tourne")
}

func TestRecover_CronBackAfterSuccessfulRun(t *testing.T) {
	w := *cronWorkload()
	w.Status, w.CurrentPhase = utils.WorkloadCrashLoop, utils.PhasePodEventPrefix+"OOMKilled"
	store := &fakeIncidents{rows: []domain.Workload{w}}
	r := newRecoverer(store, &recordingPublisher{}, job("a", time.Hour, "r/p:1", batchv1.JobComplete, ""))

	n, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestRecover_CronStillFailingStays(t *testing.T) {
	w := *cronWorkload()
	w.Status, w.CurrentPhase = utils.WorkloadCrashLoop, utils.PhasePodEventPrefix+"OOMKilled"
	store := &fakeIncidents{rows: []domain.Workload{w}}
	r := newRecoverer(store, &recordingPublisher{}, job("a", time.Hour, "r/p:1", batchv1.JobFailed, "BackoffLimitExceeded"))

	n, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

type recordingAlerts struct{ statuses []string }

func (a *recordingAlerts) WorkloadIncident(_ context.Context, _, _ uuid.UUID, _, status string) {
	a.statuses = append(a.statuses, status)
}

func TestSweepCrons_FailedRunMarksAndAlerts(t *testing.T) {
	w := *cronWorkload()
	w.Status, w.CurrentPhase = utils.WorkloadRunning, utils.PhaseDeployed
	store := &fakeIncidents{crons: []domain.Workload{w}}
	pub := &recordingPublisher{}
	alerts := &recordingAlerts{}
	r := newRecoverer(store, pub, job("a", time.Hour, "r/p:1", batchv1.JobFailed, "DeadlineExceeded"))
	r.Alerts = alerts

	n, err := r.SweepCrons(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{utils.PhasePodEventPrefix + "DeadlineExceeded"}, store.failedPhases)
	assert.Equal(t, []string{utils.WorkloadRunFailed}, alerts.statuses)
	assert.Len(t, pub.channels, 2)
}

func TestSweepCrons_SuccessOrRunningUntouched(t *testing.T) {
	for name, j := range map[string]*batchv1.Job{
		"réussie":  job("a", time.Hour, "r/p:1", batchv1.JobComplete, ""),
		"en cours": job("a", time.Minute, "r/p:1", "", ""),
	} {
		w := *cronWorkload()
		w.Status = utils.WorkloadRunning
		store := &fakeIncidents{crons: []domain.Workload{w}}
		r := newRecoverer(store, &recordingPublisher{}, j)
		n, err := r.SweepCrons(context.Background())
		require.NoError(t, err, name)
		assert.Zero(t, n, name)
		assert.Empty(t, store.failedPhases, name)
	}
}

func TestSweepCrons_StatusChangedMeanwhile(t *testing.T) {
	w := *cronWorkload()
	store := &fakeIncidents{crons: []domain.Workload{w}, changed: true}
	alerts := &recordingAlerts{}
	r := newRecoverer(store, &recordingPublisher{}, job("a", time.Hour, "r/p:1", batchv1.JobFailed, "BackoffLimitExceeded"))
	r.Alerts = alerts
	n, err := r.SweepCrons(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, alerts.statuses, "pas d'alerte sans changement d'état")
}
