package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TaskSendPush = "push:send_notification"

// PayloadSendPush holds everything needed to dispatch an FCM push notification.
// Tokens are pre-fetched by the caller; the handler does not need DB access.
type PayloadSendPush struct {
	UserID string            `json:"user_id"`
	Tokens []string          `json:"tokens"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Data   map[string]string `json:"data,omitempty"`
}

func (d *RedisTaskDistributor) DistributeTaskSendPush(ctx context.Context, payload *PayloadSendPush, opts ...asynq.Option) error {
	if len(payload.Tokens) == 0 && payload.UserID == "" {
		return fmt.Errorf("PayloadSendPush: Tokens or UserID is required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal PayloadSendPush: %w", err)
	}
	defaults := []asynq.Option{
		asynq.Queue(QueueDefault),
		asynq.MaxRetry(3),
		asynq.Timeout(60 * 1e9), // 60 s max per push attempt
	}
	return d.enqueue(ctx, TaskSendPush, b, append(defaults, opts...))
}
