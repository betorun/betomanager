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
	"strings"
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

func TestE2E_Cards(t *testing.T) {
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
	inv, err := s.Create(ctx, store.Investigation{SubjectKind: store.SubjectCluster, ActorUserID: "u1", ActorEmail: "awa@betorun.test", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	card := store.Card{InvestigationID: inv.ID, Kind: "node.service.restart", Params: json.RawMessage(`{"node":"server-1","service":"k3s"}`),
		Target: "server-1 / k3s", Summary: "Redémarrer k3s.", Rationale: "etcd plein", Risk: "high", Confirm: "server-1"}
	c, dup, err := s.ProposeCard(ctx, card)
	if err != nil || dup {
		t.Fatal(err, dup)
	}
	// Mêmes paramètres, autre ordre des clés : la même carte.
	card.Params = json.RawMessage(`{"service":"k3s", "node":"server-1"}`)
	if again, dup, err := s.ProposeCard(ctx, card); err != nil || !dup || again.ID != c.ID {
		t.Fatalf("doublon : %v %v %v", again.ID, dup, err)
	}

	// Deux validations simultanées : une seule passe.
	var wins, lost int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.DecideCard(ctx, inv.ID, c.ID, true, "awa@betorun.test", "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, store.ErrCardDecided):
				lost++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || lost != 7 {
		t.Fatalf("validations : %d gagnées, %d refusées", wins, lost)
	}
	if err := s.FinishCard(ctx, c.ID, true, "redémarré"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCard(ctx, c.ID, true, "encore"); !errors.Is(err, store.ErrCardDecided) {
		t.Fatalf("deuxième fin : %v", err)
	}
	if _, err := s.DecideCard(ctx, inv.ID, uuid.New(), true, "x", ""); !errors.Is(err, store.ErrCardNotFound) {
		t.Fatalf("inconnue : %v", err)
	}
	if _, err := s.DecideCard(ctx, uuid.New(), c.ID, true, "x", ""); !errors.Is(err, store.ErrCardNotFound) {
		t.Fatalf("autre dossier : %v", err)
	}

	// Plafond des cartes en attente.
	for i := range store.MaxOpenCards {
		card.Params = json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)
		if _, _, err := s.ProposeCard(ctx, card); err != nil {
			t.Fatal(err)
		}
	}
	card.Params = json.RawMessage(`{"n":9}`)
	if _, _, err := s.ProposeCard(ctx, card); !errors.Is(err, store.ErrTooManyCards) {
		t.Fatalf("plafond : %v", err)
	}
	cards, err := s.Cards(ctx, inv.ID)
	if err != nil || len(cards) != 1+store.MaxOpenCards || cards[0].Status != store.CardExecuted || cards[0].DecidedBy != "awa@betorun.test" || cards[0].Result != "redémarré" {
		t.Fatalf("cartes : %d %+v %v", len(cards), cards[0], err)
	}

	// Étape « action » acceptée par la contrainte.
	if _, err := s.AddStep(ctx, inv.ID, store.Step{Kind: store.StepAction, Tool: "node.service.restart", Digest: "Carte exécutée."}); err != nil {
		t.Fatal(err)
	}
	if err := s.Conclude(ctx, inv.ID, store.StatusConcluded, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ProposeCard(ctx, card); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("dossier clos : %v", err)
	}
}

// Issue relue de l'API après l'expiration locale ; carte validée restée sans
// issue (arrêt pendant l'action) close par le ramasseur.
func TestE2E_CardsSettleAndReap(t *testing.T) {
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
	now := time.Now().UTC()
	s.SetNow(func() time.Time { return now })
	inv, err := s.Create(ctx, store.Investigation{SubjectKind: store.SubjectCluster, ActorUserID: "u1", ActorEmail: "awa@betorun.test", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	card := store.Card{InvestigationID: inv.ID, Kind: "api.add_ticket_note", Params: json.RawMessage(`{"api_card":"x"}`),
		Target: "ticket", Summary: "Note.", Rationale: "trace", Risk: "low"}
	c, _, err := s.ProposeCard(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(store.CardTTL + time.Minute)
	if _, err := s.DecideCard(ctx, inv.ID, c.ID, true, "awa@betorun.test", ""); !errors.Is(err, store.ErrCardDecided) {
		t.Fatalf("validation locale après expiration : %v", err)
	}
	got, err := s.SettleCard(ctx, inv.ID, c.ID, store.CardExecuted, "awa@betorun.test", "validée auprès de l'API", "fait")
	if err != nil || got.Status != store.CardExecuted || got.Result != "fait" || got.ExecutedAt == nil {
		t.Fatalf("issue relue : %+v %v", got, err)
	}
	if _, err := s.SettleCard(ctx, inv.ID, c.ID, store.CardFailed, "x", "", ""); !errors.Is(err, store.ErrCardDecided) {
		t.Fatalf("deuxième issue : %v", err)
	}
	if _, err := s.SettleCard(ctx, inv.ID, c.ID, store.CardApproved, "x", "", ""); err == nil {
		t.Fatal("issue « validée » acceptée")
	}

	card.Kind, card.Params = "node.service.restart", json.RawMessage(`{"node":"a","service":"nginx"}`)
	stuck, _, err := s.ProposeCard(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideCard(ctx, inv.ID, stuck.ID, true, "awa@betorun.test", ""); err != nil {
		t.Fatal(err)
	}
	if reaped, err := s.ReapInterrupted(ctx, 4*time.Minute, "interrompue"); err != nil || len(reaped) != 0 {
		t.Fatalf("ramassée trop tôt : %v %v", reaped, err)
	}
	now = now.Add(5 * time.Minute)
	reaped, err := s.ReapInterrupted(ctx, 4*time.Minute, "interrompue")
	if err != nil || len(reaped) != 1 || reaped[0].ID != stuck.ID || reaped[0].Status != store.CardFailed || reaped[0].Result != "interrompue" {
		t.Fatalf("ramasseur : %+v %v", reaped, err)
	}
	if err := s.FinishCard(ctx, stuck.ID, true, "tard"); !errors.Is(err, store.ErrCardDecided) {
		t.Fatalf("fin après ramassage : %v", err)
	}

	// Conversation : sans les étapes d'outils ni leurs résultats.
	big := strings.Repeat("x", 1<<20)
	for _, st := range []store.Step{{Kind: store.StepQuestion, Digest: "Pourquoi ?"},
		{Kind: store.StepTool, Tool: "node_list", Digest: "2 nœuds", Result: &big},
		{Kind: store.StepAnswer, Digest: "Parce que."}} {
		if _, err := s.AddStep(ctx, inv.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	_, conv, err := s.Conversation(ctx, inv.ID)
	if err != nil || len(conv) != 2 || conv[0].Digest != "Pourquoi ?" || conv[1].Kind != store.StepAnswer || conv[0].Result != nil {
		t.Fatalf("conversation : %+v %v", conv, err)
	}
}
