package mail

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
)

// MailTaskEnqueuer implements mail/service.EventPublisher by routing to the
// asynq task distributor instead of publishing to PubSub directly.
type MailTaskEnqueuer struct {
	dist queue.TaskDistributor
}

func NewMailTaskEnqueuer(dist queue.TaskDistributor) *MailTaskEnqueuer {
	return &MailTaskEnqueuer{dist: dist}
}

// Publish implements mail/service.EventPublisher. topic is ignored; the task
// type is fixed to queue.TaskSendMail.
func (e *MailTaskEnqueuer) Publish(ctx context.Context, _ string, data []byte) error {
	var event mailSvc.MailEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("mail enqueuer: unmarshal: %w", err)
	}
	return e.dist.DistributeTaskSendMail(ctx, &queue.PayloadSendMail{
		To:          event.To,
		Attachments: event.Attachments,
		Subject:     event.Subject,
		Template:    event.Template,
		Variables:   event.Variables,
		Ref:         event.Ref,
	})
}
