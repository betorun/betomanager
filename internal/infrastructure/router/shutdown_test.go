package router

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
	"go.uber.org/goleak"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/cache"
)

// À l'arrêt, les clients ouverts par l'application sont fermés : Redis (le
// client de l'App, pas le global de beto-shared, jamais initialisé) et le
// cache local d'authentification (goroutines ristretto).
func TestShutdownInfra_ClosesClients(t *testing.T) {
	defer goleak.VerifyNone(t)
	ac, err := cache.NewRistrettoAuthCacheWithSize(1e4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	a := &App{Logger: zap.NewNop().Sugar(), Redis: rdb, AuthCache: ac}
	a.shutdownInfra()
	if err := rdb.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("Redis encore ouvert après l'arrêt : %v", err)
	}
}
