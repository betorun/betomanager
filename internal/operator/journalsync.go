package operator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// JournalSource : le journal à recopier (store.Store).
type JournalSource interface {
	UnsyncedJournal(ctx context.Context, limit int) ([]store.Entry, error)
	MarkSynced(ctx context.Context, ids []int64) error
}

// JournalSync : recopie le journal du serveur de gestion dans l'audit de la
// plateforme dès que l'API répond (ADR 0016 §1). Lots signés (Ed25519),
// importés une seule fois par entrée côté API : une coupure au milieu d'un
// envoi se reprend sans doublon.
type JournalSync struct {
	Source  JournalSource
	BaseURL string // OPERATOR_API_URL, ex. https://…/kmanager/v1
	Key     ed25519.PrivateKey
	HTTP    *http.Client
	Logger  *slog.Logger
	Now     func() time.Time
}

// Once : recopie tout ce qui attend, lot par lot. Rend le nombre d'entrées
// recopiées ; une erreur arrête le passage (l'entrée reste à recopier).
func (j *JournalSync) Once(ctx context.Context) (int, error) {
	total := 0
	for {
		entries, err := j.Source.UnsyncedJournal(ctx, opjournal.MaxEntries)
		if err != nil || len(entries) == 0 {
			return total, err
		}
		if err := j.send(ctx, entries); err != nil {
			return total, err
		}
		ids := make([]int64, len(entries))
		for i, e := range entries {
			ids[i] = e.ID
		}
		if err := j.Source.MarkSynced(ctx, ids); err != nil {
			return total, err
		}
		total += len(entries)
		if len(entries) < opjournal.MaxEntries {
			return total, nil
		}
	}
}

func (j *JournalSync) send(ctx context.Context, entries []store.Entry) error {
	batch := opjournal.Batch{Entries: make([]opjournal.Entry, len(entries))}
	for i, e := range entries {
		batch.Entries[i] = opjournal.Entry{UID: e.UID, At: e.At, Actor: e.Actor, Kind: e.Kind,
			InvestigationID: e.InvestigationID, Detail: e.Detail}
	}
	body, err := opjournal.Encode(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.BaseURL+opjournal.Path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range opjournal.Sign(j.Key, j.now(), body) {
		req.Header.Set(k, v)
	}
	hc := j.HTTP
	if hc == nil {
		hc = journalClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("operator journal sync: HTTP %d", resp.StatusCode)
	}
	var res opjournal.Result
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&res); err != nil || res.Accepted != len(entries) {
		return fmt.Errorf("operator journal sync: unexpected answer")
	}
	return nil
}

// Run : un passage toutes les `every`, jusqu'à l'arrêt. Une API en panne
// est le cas normal du secours : l'échec n'est signalé qu'une fois par
// série d'échecs.
func (j *JournalSync) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	failing := false
	for {
		n, err := j.Once(ctx)
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			failing = true
			j.Logger.Warn("operator journal sync paused", "error", err)
		case err == nil:
			if failing {
				j.Logger.Info("operator journal sync resumed")
			}
			failing = false
			if n > 0 {
				j.Logger.Info("operator journal copied to audit", "entries", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (j *JournalSync) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}
