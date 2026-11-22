package service

import (
	"context"
	"testing"

	"github.com/thekrauss/kubemanager/internal/modules/diskresize/domain"
)

// cancelledSpawner : exécute la tâche dans l'appel, arrêt déjà demandé.
type cancelledSpawner struct{ names []string }

func (s *cancelledSpawner) Go(name string, fn func(context.Context)) {
	s.names = append(s.names, name)
	stop, cancel := context.WithCancel(context.Background())
	cancel()
	fn(stop)
}

// Le redimensionnement passe par le Spawner (suivi par l'arrêt) mais ne
// s'annule PAS à l'arrêt : couper une copie ou un échange de disque en
// cours serait pire que le laisser finir ; un arrêt plus long que le délai
// est repris par la logique existante (« en cours » sans verrou).
func TestResize_SpawnerTracksButDoesNotCancel(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	s.Sync = false
	sp := &cancelledSpawner{}
	s.Spawner = sp
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if len(sp.names) != 1 || sp.names[0] != "disk-resize" {
		t.Fatalf("spawner = %v", sp.names)
	}
	if got := st.rows[0]; got.Status != domain.StatusCompleted || cl.size["api-pvc"] != "5Gi" {
		t.Fatalf("resize = %+v, disk = %s", got, cl.size["api-pvc"])
	}
}
