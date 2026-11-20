package router

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/background"
)

// Une tâche lancée par un module voit l'arrêt, et l'arrêt attend qu'elle
// ait fini (ici : qu'elle ait « enregistré » son état).
func TestSpawner_StopCancelsAndWaits(t *testing.T) {
	a := &App{Logger: zap.NewNop().Sugar()}
	started := make(chan struct{})
	recorded := false
	background.Go(a.spawner(), "task", func(stop context.Context) {
		close(started)
		<-stop.Done()
		recorded = true
	})
	<-started
	a.stopBackground()
	if !recorded {
		t.Fatal("l'arrêt n'a pas attendu la tâche")
	}
}
