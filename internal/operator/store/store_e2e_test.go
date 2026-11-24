package store_test

// Base de l'Opérateur sur un vrai Postgres. Ignoré sans OPERATOR_E2E_DSN :
//
//	bash scripts/operator_e2e_db.sh
//	OPERATOR_E2E_DSN=postgres://postgres:pw@127.0.0.1:55434/postgres?sslmode=disable go test ./internal/operator/store/ -count=1

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/operator/store"
)

func TestE2E_Investigation(t *testing.T) {
	dsn := os.Getenv("OPERATOR_E2E_DSN")
	if dsn == "" {
		t.Skip("OPERATOR_E2E_DSN not set")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := store.Migrate(dsn); err != nil { // idempotent
		t.Fatal(err)
	}

	inv, err := s.Create(ctx, store.Investigation{SubjectKind: store.SubjectTicket, SubjectRef: "4f2", Reason: "ticket 4f2 : site en panne",
		ActorUserID: "u1", ActorEmail: "awa@betorun.test", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddStep(ctx, inv.ID, store.Step{Kind: store.StepQuestion, Digest: "Pourquoi km-web est en panne ?"}); err != nil {
		t.Fatal(err)
	}
	// Étapes concurrentes : numéros consécutifs sans collision.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := "OOMKilled x3"
			_, err := s.AddStep(ctx, inv.ID, store.Step{Kind: store.StepTool, Tool: "list_pods",
				Input: json.RawMessage(`{"namespace":"km-web"}`), Digest: "3 redémarrages", Result: &res, DurationMs: 12})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddUsage(ctx, inv.ID, 200, 50); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, uuid.New(), 1, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("usage sur dossier inconnu : %v", err)
	}
	if err := s.Conclude(ctx, inv.ID, store.StatusConcluded, "Limite mémoire trop basse.", 1000, 250); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddStep(ctx, inv.ID, store.Step{Kind: store.StepAnswer}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("étape sur dossier clos : %v", err)
	}
	if err := s.Conclude(ctx, inv.ID, store.StatusAbandoned, "", 0, 0); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("double clôture : %v", err)
	}
	if err := s.Conclude(ctx, uuid.New(), store.StatusConcluded, "", 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dossier inconnu : %v", err)
	}

	got, steps, err := s.Get(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusConcluded || got.InputTokens != 1200 || got.ConcludedAt == nil || got.Reason != inv.Reason {
		t.Fatalf("dossier = %+v", got)
	}
	if len(steps) != 9 {
		t.Fatalf("%d étapes", len(steps))
	}
	for i, st := range steps {
		if st.Seq != i+1 {
			t.Fatalf("numéros : %v", steps)
		}
	}
	if steps[0].Input != nil || steps[1].Result == nil || string(steps[1].Input) != `{"namespace": "km-web"}` {
		t.Fatalf("étapes = %+v / %s", steps[:2], steps[1].Input)
	}

	n, err := s.PurgeResults(ctx, time.Now().Add(time.Minute))
	if err != nil || n < 8 {
		t.Fatalf("purge : %d %v", n, err)
	}
	_, steps, _ = s.Get(ctx, inv.ID)
	if steps[1].Result != nil || steps[1].Digest != "3 redémarrages" {
		t.Fatal("purge : condensé perdu ou résultat gardé")
	}
	list, err := s.List(ctx, 10)
	if err != nil || len(list) == 0 || list[0].ID != inv.ID {
		t.Fatalf("liste : %v %v", list, err)
	}
}

func TestE2E_JournalSync(t *testing.T) {
	dsn := os.Getenv("OPERATOR_E2E_DSN")
	if dsn == "" {
		t.Skip("OPERATOR_E2E_DSN not set")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Repartir de rien : d'autres tests écrivent dans le journal.
	pending, err := s.UnsyncedJournal(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var old []int64
	for _, e := range pending {
		old = append(old, e.ID)
	}
	if err := s.MarkSynced(ctx, old); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"a.un", "a.deux", "a.trois"} {
		if err := s.Journal(ctx, "cli", kind, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UnsyncedJournal(ctx, 2)
	if err != nil || len(got) != 2 || got[0].Kind != "a.un" || got[1].Kind != "a.deux" || got[0].UID == uuid.Nil || got[0].UID == got[1].UID {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.MarkSynced(ctx, []int64{got[0].ID, got[1].ID}); err != nil {
		t.Fatal(err)
	}
	rest, err := s.UnsyncedJournal(ctx, 10)
	if err != nil || len(rest) != 1 || rest[0].Kind != "a.trois" {
		t.Fatalf("%+v %v", rest, err)
	}
}
