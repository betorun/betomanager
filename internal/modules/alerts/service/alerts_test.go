package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	buildsDomain "github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

type fakeStore struct {
	mu      sync.Mutex
	claimed map[string]bool
	owners  []domain.Recipient
	muted   map[string]map[uuid.UUID]bool // catégorie → personnes
}

func (f *fakeStore) Muted(_ context.Context, category string, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	return f.muted[category], nil
}

func (f *fakeStore) Claim(_ context.Context, subject uuid.UUID, kind string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := subject.String() + kind
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}
func (f *fakeStore) Recipients(context.Context, uuid.UUID) ([]domain.Recipient, error) {
	return f.owners, nil
}
func (f *fakeStore) ProjectName(context.Context, uuid.UUID) (string, error) { return "boutique", nil }
func (f *fakeStore) OrgRecipients(context.Context, uuid.UUID) ([]domain.Recipient, error) {
	return f.owners, nil
}
func (f *fakeStore) OrgName(context.Context, uuid.UUID) (string, error) { return "Bomba", nil }

type fakeMail struct{ sent []mail.Message }

func (f *fakeMail) Send(_ context.Context, m mail.Message) error {
	f.sent = append(f.sent, m)
	return nil
}

func newNotifier() (*Notifier, *fakeMail) {
	m := &fakeMail{}
	return &Notifier{Store: &fakeStore{claimed: map[string]bool{}, owners: []domain.Recipient{
		{Email: "awa@example.com", FullName: "Awa"}, {Email: "moussa@example.com"}}},
		Mail: m, ConsoleURL: "https://console.example.com/", Sync: true}, m
}

func TestWorkloadIncident_SentToOwnersOnce(t *testing.T) {
	n, m := newNotifier()
	project, workload := uuid.New(), uuid.New()

	n.WorkloadIncident(context.Background(), project, workload, "api", utils.WorkloadOOMKilled)
	n.WorkloadIncident(context.Background(), project, workload, "api", utils.WorkloadCrashLoop)

	require.Len(t, m.sent, 2, "un email par owner, une seule alerte par période")
	msg := m.sent[0]
	assert.Equal(t, "awa@example.com", msg.To)
	assert.Equal(t, "« api » a manqué de mémoire", msg.Subject)
	assert.Contains(t, msg.Text, "Bonjour Awa,")
	assert.Contains(t, msg.Text, "« boutique »")
	assert.Contains(t, msg.Text, "https://console.example.com/workloads/"+workload.String())
	assert.Contains(t, m.sent[1].Text, "Bonjour moussa,", "sans nom : partie locale de l'adresse")
}

func TestWorkloadIncident_IgnoresOtherStatuses(t *testing.T) {
	n, m := newNotifier()
	n.WorkloadIncident(context.Background(), uuid.New(), uuid.New(), "api", utils.WorkloadRunning)
	assert.Empty(t, m.sent)
}

func TestBuildFailed(t *testing.T) {
	n, m := newNotifier()
	project, build := uuid.New(), uuid.New()

	n.BuildFailed(context.Background(), project, build, buildsDomain.FailureSuperseded)
	assert.Empty(t, m.sent, "build remplacé : rien à faire pour le client")

	n.BuildFailed(i18n.With(context.Background(), i18n.EN), project, build, buildsDomain.FailureBuild)
	require.Len(t, m.sent, 2)
	assert.Equal(t, `Build failed in "boutique"`, m.sent[0].Subject)
	assert.Contains(t, m.sent[0].Text, "https://console.example.com/builds/"+build.String())
}

// Noms choisis par le client : échappés dans la version HTML.
func TestRender_EscapesHTML(t *testing.T) {
	msg, err := render(context.Background(), workloadCopies[utils.WorkloadCrashLoop],
		domain.Recipient{Email: "a@example.com", FullName: "<b>Awa</b>"}, "<script>x</script>", "api", "https://c/w")
	require.NoError(t, err)
	assert.NotContains(t, msg.HTML, "<script>")
	assert.NotContains(t, msg.HTML, "<b>Awa</b>")
	assert.True(t, strings.Contains(msg.HTML, "&lt;script&gt;"))
}

// Chaque texte existe dans les deux langues et se remplit.
func TestCopies_Complete(t *testing.T) {
	for _, set := range []map[string]map[i18n.Locale]alertCopy{workloadCopies, buildCopies} {
		for key, copies := range set {
			for _, lang := range []i18n.Locale{i18n.FR, i18n.EN} {
				c := copies[lang]
				require.NotEmpty(t, c.Subject, "%s %s", key, lang)
				require.NotEmpty(t, c.Body, "%s %s", key, lang)
				_, err := render(i18n.With(context.Background(), lang), copies, domain.Recipient{Email: "a@example.com"}, "p", "w", "https://c")
				require.NoError(t, err, "%s %s", key, lang)
			}
		}
	}
}
