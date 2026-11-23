package service

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// Relais de l'outbox : les demandes écrites par l'API sont lancées en
// workflows Temporal (WorkflowID = ID de la demande). Temporal en panne : les
// demandes restent « en attente » et partent à son retour, sans doublon. Pas
// d'exécution de secours ailleurs (risque de double exécution).

// DefaultRelayEvery : fréquence du relais.
const DefaultRelayEvery = 2 * time.Second

// relayBatch : demandes lancées au plus par passage.
const relayBatch = 50

// startTimeout : un lancement ; passTimeout : un passage (la transaction qui
// verrouille les demandes reste courte, même Temporal muet) ; maxBackoff :
// attente maximale entre deux passages quand Temporal refuse tout.
const (
	startTimeout = 5 * time.Second
	passTimeout  = 30 * time.Second
	maxBackoff   = time.Minute
)

// ErrAlreadyStarted : le workflow de cet identifiant existe déjà (lancé par un
// passage précédent) ; la demande est marquée lancée.
var ErrAlreadyStarted = errors.New("compute request already started")

// RequestInput : ce que reçoit le workflow (jamais de secret : seulement des
// références lues en base par les activités).
type RequestInput struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
}

// Relay : lance les demandes en attente.
type Relay struct {
	Store   Store
	Starter Starter
	Logger  *zap.SugaredLogger
	Now     func() time.Time
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Run : passages réguliers jusqu'à l'arrêt ; Temporal injoignable, les
// passages s'espacent (jusqu'à maxBackoff) puis reprennent leur rythme.
func (r *Relay) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultRelayEvery
	}
	wait := every
	for {
		_, failed, err := r.pass(ctx)
		if err != nil && ctx.Err() == nil {
			r.Logger.Warnw("compute relay pass failed", "error", err)
		}
		if failed > 0 || err != nil {
			wait = min(wait*2, maxBackoff)
		} else {
			wait = every
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Pass : un passage ; rend le nombre de demandes lancées.
func (r *Relay) Pass(ctx context.Context) (int, error) {
	started, _, err := r.pass(ctx)
	return started, err
}

func (r *Relay) pass(ctx context.Context) (started, failed int, err error) {
	passCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	err = r.Store.ClaimPending(ctx, relayBatch, func(tx *gorm.DB, rs []domain.Request) error {
		for _, req := range rs {
			if passCtx.Err() != nil {
				break // délai du passage écoulé : le reste attend le suivant
			}
			startCtx, cancelStart := context.WithTimeout(passCtx, startTimeout)
			err := r.Starter.Start(startCtx, req.ID.String(), req.Kind, RequestInput{RequestID: req.ID.String(), Kind: req.Kind})
			cancelStart()
			switch {
			case err == nil, errors.Is(err, ErrAlreadyStarted):
				if err := r.Store.MarkStarted(tx, req.ID, r.now()); err != nil {
					return err
				}
				started++
			default:
				// Temporal injoignable : la demande attend le passage suivant.
				failed++
				r.Logger.Warnw("compute request not started (will retry)", "request", req.ID, "kind", req.Kind, "error", err)
				if err := r.Store.MarkAttemptFailed(tx, req.ID, err.Error()); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return started, failed, err
}
