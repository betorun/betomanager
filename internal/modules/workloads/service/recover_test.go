package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

type fakeIncidents struct {
	rows         []domain.Workload
	crons        []domain.Workload
	failedPhases []string
	changed      bool // statut modifié entre la lecture et l'écriture
	recovered    []uuid.UUID
}

func (f *fakeIncidents) ListIncidents(context.Context, int) ([]domain.Workload, error) {
	return f.rows, nil
}

func (f *fakeIncidents) ListRunningCrons(context.Context, int) ([]domain.Workload, error) {
	return f.crons, nil
}

func (f *fakeIncidents) MarkRunFailed(_ context.Context, _ uuid.UUID, _, newPhase string) (bool, error) {
	if f.changed {
		return false, nil
	}
	f.failedPhases = append(f.failedPhases, newPhase)
	return true, nil
}

func (f *fakeIncidents) Recover(_ context.Context, id uuid.UUID, _, _, _ string) (bool, error) {
	if f.changed {
		return false, nil
	}
	f.recovered = append(f.recovered, id)
	return true, nil
}

type recordingPublisher struct{ channels []string }

func (p *recordingPublisher) Publish(_ context.Context, ch string, _ any) error {
	p.channels = append(p.channels, ch)
	return nil
}

var recoverNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func crashedWorkload() domain.Workload {
	return domain.Workload{ID: uuid.New(), ProjectID: uuid.New(), Name: "web", Namespace: "km-x",
		Status: utils.WorkloadCrashLoop, CurrentPhase: utils.PhasePodEventPrefix + "CrashLoopBackOff"}
}

func readyDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "km-x", Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: replicas, UpdatedReplicas: replicas,
			ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
}

// pod : prêt et démarré il y a `age`.
func pod(name string, age time.Duration) *corev1.Pod {
	since := metav1.NewTime(recoverNow.Add(-age))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "km-x", Labels: map[string]string{utils.LabelApp: "web"}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: since}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: since}}}},
		},
	}
}

func newRecoverer(store *fakeIncidents, pub *recordingPublisher, objs ...runtime.Object) *Recoverer {
	return &Recoverer{Store: store, K8s: fake.NewSimpleClientset(objs...), Publisher: pub,
		Logger: zap.NewNop().Sugar(), Now: func() time.Time { return recoverNow }}
}

func TestRecover_StableWorkloadBackToRunning(t *testing.T) {
	w := crashedWorkload()
	store := &fakeIncidents{rows: []domain.Workload{w}}
	pub := &recordingPublisher{}
	r := newRecoverer(store, pub, readyDeployment(1), pod("web-1", 5*time.Minute))

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []uuid.UUID{w.ID}, store.recovered)
	assert.Len(t, pub.channels, 2, "canal du conteneur et canal du projet")
}

// Prêt depuis quelques secondes : un conteneur en CrashLoopBackOff l'est
// souvent entre deux plantages.
func TestRecover_RecentlyRestartedStaysInIncident(t *testing.T) {
	store := &fakeIncidents{rows: []domain.Workload{crashedWorkload()}}
	r := newRecoverer(store, &recordingPublisher{}, readyDeployment(1), pod("web-1", 20*time.Second))

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, store.recovered)
}

func TestRecover_NotAllReplicasReady(t *testing.T) {
	dep := readyDeployment(2)
	dep.Status.ReadyReplicas = 1
	dep.Status.UnavailableReplicas = 1
	store := &fakeIncidents{rows: []domain.Workload{crashedWorkload()}}
	r := newRecoverer(store, &recordingPublisher{}, dep, pod("web-1", time.Hour))

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Zero(t, n)
}

// Un pod expulsé reste listé en Failed : il ne doit pas bloquer le retour.
func TestRecover_IgnoresEvictedPod(t *testing.T) {
	evicted := pod("web-old", time.Hour)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	w := crashedWorkload()
	w.Status, w.CurrentPhase = utils.WorkloadEvicted, utils.PhasePodEventPrefix+"Evicted"
	store := &fakeIncidents{rows: []domain.Workload{w}}
	r := newRecoverer(store, &recordingPublisher{}, readyDeployment(1), evicted, pod("web-new", 5*time.Minute))

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// Un déploiement lancé entre la lecture et l'écriture garde la main : rien
// n'est diffusé.
func TestRecover_StatusChangedMeanwhile(t *testing.T) {
	store := &fakeIncidents{rows: []domain.Workload{crashedWorkload()}, changed: true}
	pub := &recordingPublisher{}
	r := newRecoverer(store, pub, readyDeployment(1), pod("web-1", 5*time.Minute))

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, pub.channels)
}

func TestRecover_DeploymentGone(t *testing.T) {
	store := &fakeIncidents{rows: []domain.Workload{crashedWorkload()}}
	r := newRecoverer(store, &recordingPublisher{})

	n, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Zero(t, n)
}

// Déploiement tombé en délai dépassé (application lente) : levé quand les pods
// sont sains ET que le Deployment porte la version voulue.
func TestRecover_DeployTimeoutRecoversWhenCurrent(t *testing.T) {
	w := crashedWorkload()
	w.Status, w.CurrentPhase, w.Image, w.EnvRevision = utils.WorkloadFailed, utils.PhaseHelmError, "reg/p/auth:b-2", 3
	dep := readyDeployment(1)
	dep.Spec.Template.Annotations = map[string]string{utils.AnnotationEnvChecksum: "3"}
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "reg/p/auth:b-2"}}
	store := &fakeIncidents{rows: []domain.Workload{w}}
	r := newRecoverer(store, &recordingPublisher{}, dep, pod("web-1", 5*time.Minute))

	n, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestRecover_DeployFailureOnOldVersionStays(t *testing.T) {
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"ancienne image":    func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "reg/p/auth:b-1" },
		"ancienne révision": func(d *appsv1.Deployment) { d.Spec.Template.Annotations[utils.AnnotationEnvChecksum] = "2" },
	} {
		w := crashedWorkload()
		w.Status, w.CurrentPhase, w.Image, w.EnvRevision = utils.WorkloadFailed, utils.PhaseHelmError, "reg/p/auth:b-2", 3
		dep := readyDeployment(1)
		dep.Spec.Template.Annotations = map[string]string{utils.AnnotationEnvChecksum: "3"}
		dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "reg/p/auth:b-2"}}
		mutate(dep)
		store := &fakeIncidents{rows: []domain.Workload{w}}
		r := newRecoverer(store, &recordingPublisher{}, dep, pod("web-1", 5*time.Minute))
		n, err := r.Sweep(context.Background())
		require.NoError(t, err, name)
		assert.Zero(t, n, name)
	}
}
