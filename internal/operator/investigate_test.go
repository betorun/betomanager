package operator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/kube"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// memStore : la base en mémoire.
type memStore struct {
	mu      sync.Mutex
	inv     store.Investigation
	steps   []store.Step
	failFor string // outil dont l'enregistrement échoue
	journal []store.Entry
}

func (m *memStore) Get(_ context.Context, id uuid.UUID) (store.Investigation, []store.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != m.inv.ID {
		return store.Investigation{}, nil, store.ErrNotFound
	}
	return m.inv, append([]store.Step(nil), m.steps...), nil
}

func (m *memStore) AddStep(_ context.Context, _ uuid.UUID, st store.Step) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.Tool != "" && st.Tool == m.failFor {
		return 0, errors.New("base indisponible")
	}
	st.Seq = len(m.steps) + 1
	m.steps = append(m.steps, st)
	return st.Seq, nil
}

func (m *memStore) Ping(context.Context) error { return nil }

func (m *memStore) Journal(_ context.Context, actor, kind string, _ *uuid.UUID, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal = append(m.journal, store.Entry{Actor: actor, Kind: kind})
	return nil
}

func (m *memStore) JournalEntries(context.Context, int) ([]store.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Entry(nil), m.journal...), nil
}

func (m *memStore) Create(_ context.Context, in store.Investigation) (store.Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in.ID, in.Status = uuid.New(), store.StatusOpen
	m.inv, m.steps = in, nil
	return in, nil
}

func (m *memStore) List(context.Context, int) ([]store.Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return []store.Investigation{m.inv}, nil
}

func (m *memStore) Conclude(_ context.Context, id uuid.UUID, st store.Status, c string, _, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != m.inv.ID {
		return store.ErrNotFound
	}
	if m.inv.Status != store.StatusOpen {
		return store.ErrClosed
	}
	m.inv.Status, m.inv.Conclusion = st, c
	return nil
}

func (m *memStore) AddUsage(_ context.Context, _ uuid.UUID, in, out int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inv.InputTokens += in
	m.inv.OutputTokens += out
	return nil
}

type quiet struct{ text strings.Builder }

func (q *quiet) Text(d string)                { q.text.WriteString(d) }
func (q *quiet) ToolStart(string, string)     {}
func (q *quiet) ToolEnd(string, string, bool) {}

var clusters agentkit.ToolRunner

func setup(model *llmtest.Provider) (*Investigator, *memStore) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "km-web", Name: "web-1"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 4,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}},
	})
	m := &memStore{inv: store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectCluster, SubjectRef: "prod-1",
		Reason: "", ActorUserID: "u1", ActorEmail: "awa@betorun.test", Status: store.StatusOpen}}
	iv := &Investigator{Store: m, Agent: &agentkit.Agent{LLM: model, Limits: Limits},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC) }}
	clusters = kube.New(map[string]kubernetes.Interface{"prod-1": cs})
	return iv, m
}

func TestAskRecordsEverything(t *testing.T) {
	model := llmtest.New("claude",
		llmtest.Call("c1", "k8s_pods", `{"cluster":"prod-1","problems_only":true}`),
		llmtest.Say("web-1 redémarre en boucle (CrashLoopBackOff, 4 redémarrages)."),
		llmtest.Say("Regarde le journal précédent."))
	iv, m := setup(model)
	q := &quiet{}
	res, err := iv.Ask(context.Background(), m.inv.ID, "Pourquoi km-web est en panne ?", clusters, q)
	if err != nil || res.Stop != agentkit.StopComplete || !strings.Contains(q.text.String(), "CrashLoopBackOff") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(m.steps) != 3 || m.steps[0].Kind != store.StepQuestion || m.steps[1].Kind != store.StepTool || m.steps[2].Kind != store.StepAnswer {
		t.Fatalf("étapes = %+v", m.steps)
	}
	tool := m.steps[1]
	if tool.Tool != "k8s_pods" || string(tool.Input) != `{"cluster":"prod-1","problems_only":true}` ||
		tool.Result == nil || !strings.Contains(*tool.Result, "web-1") || tool.Digest == "" {
		t.Fatalf("étape d'outil = %+v", tool)
	}
	if m.inv.InputTokens == 0 {
		t.Fatal("consommation non comptée")
	}
	sys := model.Requests()[0].System
	if !strings.Contains(sys, "prod-1") || !strings.Contains(sys, "ne lire aucune donnée de client") {
		t.Fatalf("prompt = %s", sys)
	}

	// Deuxième question : l'historique ne garde que les échanges en texte.
	if _, err := iv.Ask(context.Background(), m.inv.ID, "Et ensuite ?", clusters, q); err != nil {
		t.Fatal(err)
	}
	msgs := model.Requests()[2].Messages
	if len(msgs) != 3 || msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant || msgs[2].Text != "Et ensuite ?" {
		t.Fatalf("historique = %+v", msgs)
	}
}

func TestAskWithholdsUnrecordedEvidence(t *testing.T) {
	model := llmtest.New("claude",
		llmtest.Call("c1", "k8s_pods", `{"cluster":"prod-1"}`),
		llmtest.Say("Je ne peux pas conclure."))
	iv, m := setup(model)
	m.failFor = "k8s_pods"
	if _, err := iv.Ask(context.Background(), m.inv.ID, "État ?", clusters, &quiet{}); err != nil {
		t.Fatal(err)
	}
	seen := model.Requests()[1].Messages
	last := seen[len(seen)-1].ToolResult
	if last == nil || !last.IsError || strings.Contains(last.Content, "web-1") {
		t.Fatalf("résultat non enregistré montré au modèle : %+v", last)
	}
}

func TestAskRefusals(t *testing.T) {
	iv, m := setup(llmtest.New("claude"))
	if _, err := iv.Ask(context.Background(), m.inv.ID, "  ", clusters, &quiet{}); err == nil {
		t.Fatal("question vide acceptée")
	}
	if _, err := iv.Ask(context.Background(), uuid.New(), "x", clusters, &quiet{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	m.inv.Status = store.StatusConcluded
	if _, err := iv.Ask(context.Background(), m.inv.ID, "x", clusters, &quiet{}); !errors.Is(err, store.ErrClosed) {
		t.Fatal(err)
	}
}

func TestAskRecordsFailure(t *testing.T) {
	iv, m := setup(llmtest.New("claude", llmtest.Fail(llm.KindUnavailable)))
	if _, err := iv.Ask(context.Background(), m.inv.ID, "État ?", clusters, &quiet{}); err == nil {
		t.Fatal("erreur attendue")
	}
	if len(m.steps) != 2 || !m.steps[1].IsError || !strings.Contains(m.steps[1].Digest, "llm_error") {
		t.Fatalf("échec non tracé : %+v", m.steps)
	}
}

func TestDigest(t *testing.T) {
	long := strings.Repeat("ligne é\n", 200)
	d := Digest(long)
	if len(d) > digestLen+60 || !strings.Contains(d, "200 lignes") {
		t.Fatalf("%q", d)
	}
	if Digest(" court ") != "court" {
		t.Fatal("court")
	}
}
