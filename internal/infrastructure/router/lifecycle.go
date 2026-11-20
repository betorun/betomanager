package router

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/background"
)

// Tâches de fond de l'application (synchro IAM, rotation des clés STS,
// purges, boucles du worker) : toutes partent d'un même contexte de vie,
// annulé à l'arrêt, et l'arrêt attend leur fin (borné par un délai). Avant,
// plusieurs tournaient sur context.Background() et ne s'arrêtaient jamais.
type lifecycle struct {
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (l *lifecycle) init(parent context.Context) {
	l.once.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		// Les valeurs du parent sont gardées, pas son annulation : l'arrêt
		// passe par stop(), après celui des serveurs.
		l.ctx, l.cancel = context.WithCancel(context.WithoutCancel(parent))
	})
}

// context : contexte de vie (créé au besoin).
func (l *lifecycle) context() context.Context {
	l.init(context.Background())
	return l.ctx
}

// spawn : lance fn dans une goroutine suivie ; une panique est journalisée
// au lieu de faire tomber le process.
func (l *lifecycle) spawn(ctx context.Context, logger *zap.SugaredLogger, name string, fn func(context.Context)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Errorw("background task panic", "task", name, "panic", r)
			}
		}()
		fn(ctx)
	}()
}

// stop : annule le contexte de vie et attend les tâches, au plus timeout.
// Rend false si des tâches tournaient encore à l'échéance.
func (l *lifecycle) stop(timeout time.Duration) bool {
	l.init(context.Background())
	l.cancel()
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// goBackground : tâche de fond liée au contexte de vie de l'application.
func (a *App) goBackground(name string, fn func(context.Context)) {
	a.life.spawn(a.life.context(), a.Logger, name, fn)
}

// goWith : tâche de fond liée à ctx (ex. le contexte du worker), suivie par
// l'arrêt comme les autres.
func (a *App) goWith(ctx context.Context, name string, fn func(context.Context)) {
	a.life.spawn(ctx, a.Logger, name, fn)
}

// stopBackground : fin des tâches de fond, à l'arrêt.
func (a *App) stopBackground() {
	timeout := 10 * time.Second
	if a.Config != nil && a.Config.Server.ShutdownTimeout > 0 {
		timeout = a.Config.Server.ShutdownTimeout
	}
	if !a.life.stop(timeout) && a.Logger != nil {
		a.Logger.Warnw("background tasks still running at shutdown", "timeout", timeout)
	}
}

// appSpawner : les tâches de fond des modules passent par le cycle de vie
// de l'application (annulées puis attendues à l'arrêt).
type appSpawner struct{ a *App }

func (s appSpawner) Go(name string, fn func(stop context.Context)) { s.a.goBackground(name, fn) }

// spawner : à donner aux modules qui lancent des tâches de fond.
func (a *App) spawner() background.Spawner { return appSpawner{a} }
