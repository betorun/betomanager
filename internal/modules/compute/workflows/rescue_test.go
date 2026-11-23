package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type rescueStore struct {
	*vmStore
	secrets map[uuid.UUID][]byte
	deleted bool
}

func (s *rescueStore) SaveSecret(_ context.Context, sec *domain.Secret) error {
	s.secrets[sec.VPSID] = sec.Ciphertext
	return nil
}
func (s *rescueStore) DeleteSecrets(_ context.Context, id uuid.UUID, _ string) error {
	delete(s.secrets, id)
	s.deleted = true
	return nil
}

type fakeRescue struct{ modes []bool }

func (f *fakeRescue) SetRescue(_ context.Context, _ string, on bool) error {
	f.modes = append(f.modes, on)
	return nil
}

type fakeMailbox struct {
	mail    *service.RescueMail
	deleted []uint32
}

func (m *fakeMailbox) FindRescue(context.Context, string, time.Time) (*service.RescueMail, error) {
	return m.mail, nil
}
func (m *fakeMailbox) Delete(_ context.Context, id uint32) error {
	m.deleted = append(m.deleted, id)
	return nil
}
func (m *fakeMailbox) Check(context.Context) (int, error) { return 0, nil }

// xorSealer : chiffrement factice pour les tests (le vrai : auth.Sealer).
type xorSealer struct{}

func (xorSealer) Seal(p []byte, _ string) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	return out, nil
}
func (x xorSealer) Open(c []byte, a string) ([]byte, error) { return x.Seal(c, a) }

func runRescue(t *testing.T, store *rescueStore, box *fakeMailbox, rescue *fakeRescue, rid uuid.UUID, kind string) error {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store, Provider: &fakeCloud{state: domain.StateRunning}, Rescue: rescue, Mailbox: box, Sealer: xorSealer{}})
	env.ExecuteWorkflow(RequestWorkflow, service.RequestInput{RequestID: rid.String(), Kind: kind})
	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError()
}

func rescueSetup(kind string) (*rescueStore, uuid.UUID, uuid.UUID) {
	vs, vid, rid := setup(domain.StateRunning, kind)
	vs.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	return &rescueStore{vmStore: vs, secrets: map[uuid.UUID][]byte{}}, vid, rid
}

func TestEnterRescue_CapturesAndDeletesMail(t *testing.T) {
	store, vid, rid := rescueSetup(domain.KindRescueEnter)
	box := &fakeMailbox{mail: &service.RescueMail{ID: 42, Credentials: domain.RescueCredentials{Username: "root", Password: "Xk7pQ2vLm9"}}}
	rescue := &fakeRescue{}
	require.NoError(t, runRescue(t, store, box, rescue, rid, domain.KindRescueEnter))
	assert.Equal(t, []bool{true}, rescue.modes)
	assert.Equal(t, domain.RescueActive, store.vps[vid].Rescue)
	assert.Equal(t, domain.RescueSecretReady, store.vps[vid].RescueSecret)
	require.NotEmpty(t, store.secrets[vid])
	assert.NotContains(t, string(store.secrets[vid]), "Xk7pQ2vLm9", "chiffré en base")
	assert.Equal(t, []uint32{42}, box.deleted, "e-mail effacé de la boîte")
}

func TestEnterRescue_MailNeverArrives(t *testing.T) {
	store, vid, rid := rescueSetup(domain.KindRescueEnter)
	require.NoError(t, runRescue(t, store, &fakeMailbox{}, &fakeRescue{}, rid, domain.KindRescueEnter))
	assert.Equal(t, domain.RescueActive, store.vps[vid].Rescue, "le secours est actif quand même")
	assert.Equal(t, domain.RescueSecretMissing, store.vps[vid].RescueSecret, "client et équipe prévenus")
}

func TestExitRescue(t *testing.T) {
	store, vid, rid := rescueSetup(domain.KindRescueExit)
	store.vps[vid].Rescue = domain.RescueActive
	store.secrets[vid] = []byte("x")
	rescue := &fakeRescue{}
	require.NoError(t, runRescue(t, store, &fakeMailbox{}, rescue, rid, domain.KindRescueExit))
	assert.Equal(t, []bool{false}, rescue.modes)
	assert.Equal(t, "", store.vps[vid].Rescue)
	assert.True(t, store.deleted, "mot de passe effacé")
}
