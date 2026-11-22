package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hibiken/asynq"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

// TaskSendMail : envoi d'un e-mail déjà rendu (texte et HTML, langue du destinataire).
const TaskSendMail = "mail:send"

// Reprises d'un envoi : 8 essais espacés (MailRetryDelay), soit environ une
// journée avant l'archive ; un serveur SMTP en panne ou mal configuré a le
// temps d'être réparé.
const (
	MailMaxRetry = 8
	mailTimeout  = 2 * time.Minute
)

// PayloadSendMail : le message, tel qu'il sera envoyé.
type PayloadSendMail struct {
	Message mail.Message `json:"message"`
}

var ErrMailRecipientRequired = errors.New("mail task: recipient is required")

func (d *RedisTaskDistributor) DistributeTaskSendMail(ctx context.Context, payload *PayloadSendMail, opts ...asynq.Option) error {
	if payload.Message.To == "" {
		return ErrMailRecipientRequired
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	defaults := []asynq.Option{asynq.Queue(QueueDefault), asynq.MaxRetry(MailMaxRetry), asynq.Timeout(mailTimeout)}
	return d.enqueue(ctx, TaskSendMail, b, append(defaults, opts...))
}

// MailRetryDelay : 30 s, 2 min, 10 min, 30 min, 1 h, puis 3 h.
func MailRetryDelay(n int) time.Duration {
	steps := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour}
	if n < len(steps) {
		return steps[n]
	}
	return 3 * time.Hour
}
