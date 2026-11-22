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

// Run : passages réguliers jusqu'à l'arrêt.
func (r *Relay) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultRelayEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warnw("compute relay pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass : un passage ; rend le nombre de demandes lancées.
func (r *Relay) Pass(ctx context.Context) (int, error) {
	started := 0
	err := r.Store.ClaimPending(ctx, relayBatch, func(tx *gorm.DB, rs []domain.Request) error {
		for _, req := range rs {
			err := r.Starter.Start(ctx, req.ID.String(), req.Kind, RequestInput{RequestID: req.ID.String(), Kind: req.Kind})
			switch {
			case err == nil, errors.Is(err, ErrAlreadyStarted):
				if err := r.Store.MarkStarted(tx, req.ID, r.now()); err != nil {
					return err
				}
				started++
			default:
				// Temporal injoignable : la demande attend le passage suivant.
				r.Logger.Warnw("compute request not started (will retry)", "request", req.ID, "kind", req.Kind, "error", err)
				if err := r.Store.MarkAttemptFailed(tx, req.ID, err.Error()); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return started, err
}
