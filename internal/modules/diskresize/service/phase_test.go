package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/thekrauss/kubemanager/internal/modules/diskresize/domain"
)

// failingStore : la base refuse d'enregistrer une phase donnée (coupure,
// base fermée par l'arrêt de l'API).
type failingStore struct {
	*memStore
	failPhase string
}

func (f *failingStore) Save(ctx context.Context, r *domain.Resize) error {
	if f.failPhase != "" && r.Phase == f.failPhase {
		return errors.New("database is closed")
	}
	return f.memStore.Save(ctx, r)
}

// Une étape ne s'exécute jamais si sa phase n'a pas été enregistrée. Avant :
// l'échec de l'enregistrement de « swapping » était seulement journalisé, le
// disque d'origine était supprimé quand même, la base restait à « copying »
// et la reprise supprimait le disque temporaire — qui portait les données.
func TestResize_UnrecordedPhaseStopsBeforeActing(t *testing.T) {
	s, wl, cl, st := setup("RUNNING")
	fs := &failingStore{memStore: st, failPhase: domain.PhaseSwapping}
	s.Store = fs
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cl.calls, "recreate 5Gi") {
		t.Fatalf("disque d'origine recréé sans phase enregistrée : %v", cl.calls)
	}
	if cl.data["api-pvc"] != "DATA" {
		t.Fatalf("données d'origine perdues : %q", cl.data["api-pvc"])
	}

	// Base revenue : la demande suivante reprend et termine.
	fs.failPhase = ""
	if _, err := s.Start(context.Background(), wl.w.ProjectID, wl.w.ID, "5Gi", "user:u"); err != nil {
		t.Fatal(err)
	}
	last := st.rows[len(st.rows)-1]
	if last.Status != domain.StatusCompleted || cl.size["api-pvc"] != "5Gi" || cl.data["api-pvc"] != "DATA" {
		t.Fatalf("reprise : %+v, disque %s %q", last, cl.size["api-pvc"], cl.data["api-pvc"])
	}
}
