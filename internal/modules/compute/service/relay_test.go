package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

type relayStore struct {
	Store
	pending []domain.Request
	started map[uuid.UUID]bool
	failed  map[uuid.UUID]string
}

func (s *relayStore) ClaimPending(_ context.Context, _ int, fn func(*gorm.DB, []domain.Request) error) error {
	return fn(nil, s.pending)
}
func (s *relayStore) MarkStarted(_ *gorm.DB, id uuid.UUID, _ time.Time) error {
	s.started[id] = true
	return nil
}
func (s *relayStore) MarkAttemptFailed(_ *gorm.DB, id uuid.UUID, msg string) error {
	s.failed[id] = msg
	return nil
}

type fakeStarter struct {
	errs  map[string]error
	calls []string
}

func (f *fakeStarter) Start(_ context.Context, id, _ string, _ any) error {
	f.calls = append(f.calls, id)
	return f.errs[id]
}

func TestRelay_Pass(t *testing.T) {
	ok, down, again := uuid.New(), uuid.New(), uuid.New()
	store := &relayStore{pending: []domain.Request{{ID: ok, Kind: "compute.ping"}, {ID: down, Kind: "compute.ping"}, {ID: again, Kind: "compute.ping"}},
		started: map[uuid.UUID]bool{}, failed: map[uuid.UUID]string{}}
	starter := &fakeStarter{errs: map[string]error{down.String(): errors.New("temporal unavailable"), again.String(): ErrAlreadyStarted}}
	r := &Relay{Store: store, Starter: starter, Logger: zap.NewNop().Sugar()}

	n, err := r.Pass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.True(t, store.started[ok])
	assert.True(t, store.started[again], "déjà lancé (passage précédent) : marqué lancé, pas relancé ailleurs")
	assert.False(t, store.started[down], "Temporal en panne : la demande reste en attente")
	assert.Contains(t, store.failed[down], "temporal unavailable")
	assert.Equal(t, []string{ok.String(), down.String(), again.String()}, starter.calls, "WorkflowID = identifiant de la demande")
}

type slowStarter struct{ calls int }

func (s *slowStarter) Start(ctx context.Context, _, _ string, _ any) error {
	s.calls++
	<-ctx.Done() // Temporal muet
	return ctx.Err()
}

// Temporal muet : chaque lancement est borné, la demande reste en attente.
func TestRelay_StartIsBounded(t *testing.T) {
	id := uuid.New()
	store := &relayStore{pending: []domain.Request{{ID: id, Kind: "compute.ping"}}, started: map[uuid.UUID]bool{}, failed: map[uuid.UUID]string{}}
	r := &Relay{Store: store, Starter: &slowStarter{}, Logger: zap.NewNop().Sugar()}
	begin := time.Now()
	started, failed, err := r.pass(context.Background())
	require.NoError(t, err)
	assert.Less(t, time.Since(begin), startTimeout+time.Second)
	assert.Equal(t, 0, started)
	assert.Equal(t, 1, failed)
	assert.False(t, store.started[id])
}
