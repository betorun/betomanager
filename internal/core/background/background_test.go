package background

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

type recorder struct{ names []string }

func (r *recorder) Go(name string, fn func(context.Context)) {
	r.names = append(r.names, name)
	fn(context.Background())
}

func TestGo_UsesSpawner(t *testing.T) {
	r := &recorder{}
	ran := false
	Go(r, "task", func(context.Context) { ran = true })
	if !ran || len(r.names) != 1 || r.names[0] != "task" {
		t.Fatalf("ran=%v names=%v", ran, r.names)
	}
}

func TestGo_WithoutSpawner(t *testing.T) {
	defer goleak.VerifyNone(t)
	done := make(chan struct{})
	Go(nil, "task", func(stop context.Context) {
		if stop.Err() != nil {
			t.Error("contexte d'arrêt déjà annulé")
		}
		close(done)
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tâche non lancée")
	}
}
