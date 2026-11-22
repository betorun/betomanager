// Package service : envoi des e-mails par la file (asynq sur Redis), livraison
// par le worker, adresses mises de côté (docs/modules/mail.md).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
)

// Suppressions : adresses à qui plus rien n'est envoyé.
type Suppressions interface {
	IsSuppressed(ctx context.Context, email string) (bool, error)
	Suppress(ctx context.Context, s domain.Suppression) error
	List(ctx context.Context, search string, limit int) ([]domain.Suppression, error)
	Remove(ctx context.Context, email string) (bool, error)
}

// Enqueuer : la file des tâches.
type Enqueuer interface {
	DistributeTaskSendMail(ctx context.Context, payload *queue.PayloadSendMail, opts ...asynq.Option) error
}

// enqueueTimeout : Redis répond vite, ou l'appelant reçoit l'erreur.
const enqueueTimeout = 3 * time.Second

// QueueSender : mail.Sender des modules. Le message (déjà rendu) part en file ;
// le worker l'envoie. Une adresse mise de côté n'est pas mise en file.
type QueueSender struct {
	Queue        Enqueuer
	Suppressions Suppressions
	Logger       *zap.SugaredLogger
}

var _ mail.Sender = (*QueueSender)(nil)

func (s *QueueSender) Send(ctx context.Context, m mail.Message) error {
	if s.Suppressions != nil {
		// Base indisponible : on envoie quand même (le worker revérifie).
		if suppressed, err := s.Suppressions.IsSuppressed(ctx, m.To); err == nil && suppressed {
			s.Logger.Warnw("mail not sent: address suppressed", "to", domain.MaskEmail(m.To))
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), enqueueTimeout)
	defer cancel()
	return s.Queue.DistributeTaskSendMail(ctx, &queue.PayloadSendMail{Message: m})
}

// Deliverer : livraison par le worker (tâche queue.TaskSendMail).
type Deliverer struct {
	SMTP         mail.Sender
	Suppressions Suppressions
	Logger       *zap.SugaredLogger
}

// HandleSendMail : envoyé, ou réessayé (erreur rendue), ou adresse mise de
// côté (fin), ou archivé sans reprise (asynq.SkipRetry : visible dans la file
// « archived », jamais perdu sans trace).
func (d *Deliverer) HandleSendMail(ctx context.Context, t *asynq.Task) error {
	var p queue.PayloadSendMail
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("mail task payload: %v: %w", err, asynq.SkipRetry)
	}
	to := domain.MaskEmail(p.Message.To)
	if d.Suppressions != nil {
		suppressed, err := d.Suppressions.IsSuppressed(ctx, p.Message.To)
		if err != nil {
			return err // base indisponible : réessayer
		}
		if suppressed {
			d.Logger.Warnw("mail dropped: address suppressed", "to", to)
			return nil
		}
	}
	err := d.SMTP.Send(ctx, p.Message)
	switch domain.Classify(err) {
	case domain.Sent:
		d.Logger.Infow("mail sent", "to", to)
		return nil
	case domain.Rejected:
		d.Logger.Warnw("mail rejected: address suppressed", "to", to, "error", err)
		if d.Suppressions != nil {
			if serr := d.Suppressions.Suppress(ctx, domain.NewSuppression(p.Message.To, domain.ReasonRejected, domain.SourceSMTP, err.Error())); serr != nil {
				return serr
			}
		}
		return nil
	case domain.Failed:
		return fmt.Errorf("%v: %w", err, asynq.SkipRetry)
	default:
		return err
	}
}

// ErrNoSuppressions : stockage des adresses mises de côté absent.
var ErrNoSuppressions = errors.New("mail suppressions are not available")

// Admin : liste et retrait des adresses mises de côté (équipe).
type Admin struct {
	Suppressions Suppressions
}

const maxListed = 200

func (a *Admin) List(ctx context.Context, search string) ([]domain.Suppression, error) {
	if a.Suppressions == nil {
		return nil, ErrNoSuppressions
	}
	return a.Suppressions.List(ctx, search, maxListed)
}

// ErrNotSuppressed : l'adresse n'était pas mise de côté.
var ErrNotSuppressed = errors.New("this address is not suppressed")

func (a *Admin) Remove(ctx context.Context, email string) error {
	if a.Suppressions == nil {
		return ErrNoSuppressions
	}
	ok, err := a.Suppressions.Remove(ctx, email)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotSuppressed
	}
	return nil
}
