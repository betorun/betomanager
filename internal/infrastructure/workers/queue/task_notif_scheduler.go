package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TaskRunNotificationScheduler = "notification:scheduler_sweep"

// PayloadRunNotificationScheduler carries no fields — the handler uses its own
// configured batch size. A fixed TaskID ensures at most one sweep is queued at a time.
type PayloadRunNotificationScheduler struct{}

func (d *RedisTaskDistributor) DistributeTaskRunNotificationScheduler(ctx context.Context, payload *PayloadRunNotificationScheduler, opts ...asynq.Option) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal PayloadRunNotificationScheduler: %w", err)
	}
	defaults := []asynq.Option{
		asynq.Queue(QueueDefault),
		asynq.MaxRetry(0),
		asynq.TaskID("notification-scheduler-sweep"), // dedup: one sweep at a time
		asynq.Timeout(5 * 60 * 1e9),                  // 5 min max
	}
	return d.enqueue(ctx, TaskRunNotificationScheduler, b, append(defaults, opts...))
}
