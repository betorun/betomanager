package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// La boucle de synchro IAM s'arrête à l'annulation, même canal ouvert.
func TestConsumeInvalidations_StopsOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t)
	ch := make(chan *redis.Message) // jamais fermé, comme sub.Channel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		consumeInvalidations(ctx, ch, "me", func(iam.Principal) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("consumeInvalidations still running after cancel")
	}
}

func TestConsumeInvalidations_FiltersOwnAndMalformed(t *testing.T) {
	defer goleak.VerifyNone(t)
	ch := make(chan *redis.Message, 4)
	ch <- &redis.Message{Payload: "me|user:a"}    // le nôtre : ignoré
	ch <- &redis.Message{Payload: "malformed"}    // ignoré
	ch <- &redis.Message{Payload: "other|user:b"} // appliqué
	close(ch)
	var got []iam.Principal
	consumeInvalidations(context.Background(), ch, "me", func(p iam.Principal) { got = append(got, p) })
	assert.Equal(t, []iam.Principal{"user:b"}, got)
}

func TestLifecycle_StopCancelsAndWaits(t *testing.T) {
	defer goleak.VerifyNone(t)
	app := &App{Logger: zap.NewNop().Sugar()}
	app.life.init(context.Background())
	var finished atomic.Int32
	for i := 0; i < 3; i++ {
		app.goBackground("loop", func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond) // fin propre après l'annulation
			finished.Add(1)
		})
	}
	assert.True(t, app.life.stop(time.Second))
	assert.Equal(t, int32(3), finished.Load(), "stop attend la fin des tâches")
}

func TestLifecycle_PanicIsRecovered(t *testing.T) {
	defer goleak.VerifyNone(t)
	app := &App{Logger: zap.NewNop().Sugar()}
	app.goBackground("boom", func(context.Context) { panic("boom") })
	assert.True(t, app.life.stop(time.Second))
}

func TestLifecycle_StopTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var l lifecycle
	l.spawn(l.context(), nil, "stuck", func(context.Context) { <-release }) // ignore ctx
	assert.False(t, l.stop(20*time.Millisecond))
}

// Le contexte de vie garde les valeurs du parent mais pas son annulation.
func TestLifecycle_DetachedFromParentCancel(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	var l lifecycle
	l.init(parent)
	cancel()
	assert.NoError(t, l.context().Err())
	assert.Equal(t, "v", l.context().Value(key{}))
	l.stop(time.Second)
	assert.Error(t, l.context().Err())
}
