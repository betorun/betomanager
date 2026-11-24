package operator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

type fakeJournal struct {
	mu      sync.Mutex
	entries []store.Entry
	synced  map[int64]bool
}

func (f *fakeJournal) UnsyncedJournal(_ context.Context, limit int) ([]store.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Entry
	for _, e := range f.entries {
		if !f.synced[e.ID] && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeJournal) MarkSynced(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.synced[id] = true
	}
	return nil
}

func TestJournalSync(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{opjournal.KeyID(pub): pub}
	var mu sync.Mutex
	got := map[uuid.UUID]bool{}
	down := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/kmanager/v1"+opjournal.Path || opjournal.Verify(keys, r.Header.Get(opjournal.HeaderKey),
			r.Header.Get(opjournal.HeaderTimestamp), r.Header.Get(opjournal.HeaderSignature), body, time.Now()) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var b opjournal.Batch
		_ = json.Unmarshal(body, &b)
		for _, e := range b.Entries {
			got[e.UID] = true
		}
		_ = json.NewEncoder(w).Encode(opjournal.Result{Accepted: len(b.Entries), Imported: len(b.Entries)})
	}))
	defer api.Close()

	src := &fakeJournal{synced: map[int64]bool{}}
	for i := 1; i <= opjournal.MaxEntries+5; i++ {
		src.entries = append(src.entries, store.Entry{ID: int64(i), UID: uuid.New(), At: time.Now(), Actor: "cli", Kind: "rescue.account.added"})
	}
	j := &JournalSync{Source: src, BaseURL: api.URL + "/kmanager/v1", Key: priv, Logger: quietLogger()}

	// API en panne : rien n'est marqué, tout reste à recopier.
	if n, err := j.Once(context.Background()); err == nil || n != 0 || len(src.synced) != 0 {
		t.Fatalf("API en panne : %d %v", n, err)
	}
	mu.Lock()
	down = false
	mu.Unlock()
	// Deux lots (MaxEntries + 5).
	if n, err := j.Once(context.Background()); err != nil || n != opjournal.MaxEntries+5 || len(got) != n || len(src.synced) != n {
		t.Fatalf("recopie : %d %v (reçues %d)", n, err, len(got))
	}
	if n, err := j.Once(context.Background()); err != nil || n != 0 {
		t.Fatalf("rien à recopier : %d %v", n, err)
	}
	// Mauvaise clé : refus, rien de marqué.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	src.entries = append(src.entries, store.Entry{ID: 999, UID: uuid.New(), At: time.Now(), Actor: "cli", Kind: "x"})
	j.Key = other
	if _, err := j.Once(context.Background()); err == nil || src.synced[999] {
		t.Fatal("clé refusée mais entrée marquée")
	}
}
