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

// memStore : les seules méthodes utiles aux activités.
type memStore struct {
	service.Store
	reqs     map[uuid.UUID]*domain.Request
	finished map[uuid.UUID]bool
}

func (m *memStore) Request(_ context.Context, id uuid.UUID) (*domain.Request, error) {
	return m.reqs[id], nil
}
func (m *memStore) FinishRequest(_ context.Context, id uuid.UUID, ok bool, _ string, _ time.Time) error {
	m.finished[id] = ok
	return nil
}

func run(t *testing.T, store *memStore, in service.RequestInput) error {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.ExecuteWorkflow(RequestWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError()
}

func TestRequestWorkflow_PingClosesRequest(t *testing.T) {
	id := uuid.New()
	store := &memStore{reqs: map[uuid.UUID]*domain.Request{id: {ID: id, Kind: KindPing}}, finished: map[uuid.UUID]bool{}}
	require.NoError(t, run(t, store, service.RequestInput{RequestID: id.String(), Kind: KindPing}))
	assert.True(t, store.finished[id])
}

func TestRequestWorkflow_UnknownKindFailsAndCloses(t *testing.T) {
	id := uuid.New()
	store := &memStore{reqs: map[uuid.UUID]*domain.Request{}, finished: map[uuid.UUID]bool{}}
	assert.Error(t, run(t, store, service.RequestInput{RequestID: id.String(), Kind: "vm.explode"}))
	ok, closed := store.finished[id]
	assert.True(t, closed, "la demande est close même en échec")
	assert.False(t, ok)
}

func TestRequestWorkflow_ReconcileWithoutProviderFails(t *testing.T) {
	id := uuid.New()
	store := &memStore{reqs: map[uuid.UUID]*domain.Request{}, finished: map[uuid.UUID]bool{}}
	err := run(t, store, service.RequestInput{RequestID: id.String(), Kind: KindReconcile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no compute provider")
	ok, closed := store.finished[id]
	assert.True(t, closed)
	assert.False(t, ok)
}
