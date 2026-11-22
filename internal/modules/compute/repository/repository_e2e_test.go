package repository_test

// VPS sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/compute/... -count=1

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

type fcfa struct{}

func (fcfa) Rate(context.Context, uuid.UUID) (domain.Rate, error) {
	return domain.Rate{Currency: "XAF", EURMilli: 655957, RoundTo: 500}, nil
}

type countingStarter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *countingStarter) Start(_ context.Context, id, _ string, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[id]++
	time.Sleep(50 * time.Millisecond) // le second relais tente pendant ce temps
	return nil
}

func TestE2E_ComputeLot1(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	store := &repository.GormStore{DB: db}
	svc := &service.Service{Store: store, Catalogs: fcfa{}}

	// Offres créées par la migration, invendables sans prix fournisseur.
	offers, err := svc.Offers(ctx, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, offers, "VPS-1 et VPS-2 existent mais ne sont pas encore tarifées")
	all, err := svc.AdminOffers(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(all), 2)
	assert.Equal(t, "VPS-1", all[0].Code)
	assert.Equal(t, int64(20000), all[0].CoefficientBP)

	// L'équipe tarife VPS-1 : 6,49 € TTC × 655,957 × 2 → 9 000 FCFA.
	_, err = svc.SaveOffer(ctx, service.OfferUpdate{Code: "vps-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649,
		CoefficientBP: 20000, RoundTo: 500, Locations: []string{"GRA", "rbx"}, Images: map[string]string{"ubuntu-24.04": "Ubuntu 24.04"}, Public: true, Position: 1}, "user:staff")
	require.NoError(t, err)
	_, err = svc.SaveOffer(ctx, service.OfferUpdate{Code: "VPS-2", CoefficientBP: 20000, RoundTo: 500, Locations: []string{"paris"}}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrInvalidOffer, "emplacement hors liste")
	offers, err = svc.Offers(ctx, uuid.New())
	require.NoError(t, err)
	require.Len(t, offers, 1)
	assert.Equal(t, int64(9000), offers[0].Monthly)
	assert.Equal(t, []string{"gra", "rbx"}, offers[0].Locations)

	// Outbox : la demande suit la transaction de l'appelant.
	rolled := &domain.Request{Kind: "compute.ping"}
	_ = db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, store.EnqueueTx(tx, rolled))
		return errors.New("la commande échoue")
	})
	r, err := store.Request(ctx, rolled.ID)
	require.NoError(t, err)
	assert.Nil(t, r, "commande annulée : demande annulée avec elle")

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		req := &domain.Request{Kind: "compute.ping"}
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return store.EnqueueTx(tx, req) }))
		ids = append(ids, req.ID)
	}

	// Deux relais en même temps : chaque demande n'est lancée qu'une fois.
	starter := &countingStarter{calls: map[string]int{}}
	relay := &service.Relay{Store: store, Starter: starter, Logger: zap.NewNop().Sugar()}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = relay.Pass(ctx) }()
	}
	wg.Wait()
	_, err = relay.Pass(ctx)
	require.NoError(t, err)
	for _, id := range ids {
		assert.Equal(t, 1, starter.calls[id.String()], "lancée une seule fois")
		r, err := store.Request(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, domain.RequestStarted, r.Status)
	}

	// Clôture idempotente : une demande terminée ne change plus.
	require.NoError(t, store.FinishRequest(ctx, ids[0], true, "", time.Now()))
	require.NoError(t, store.FinishRequest(ctx, ids[0], false, "trop tard", time.Now()))
	r, err = store.Request(ctx, ids[0])
	require.NoError(t, err)
	assert.Equal(t, domain.RequestDone, r.Status)
}
