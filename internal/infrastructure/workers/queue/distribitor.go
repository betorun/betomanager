package queue

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

// TaskDistributor is the interface for enqueueing background tasks into Redis.
// Each DistributeTaskXxx method validates the payload, serialises it, and calls
// asynq.Client.EnqueueContext with sane defaults; callers can override with opts.
type TaskDistributor interface {
	DistributeTaskSendPush(ctx context.Context, payload *PayloadSendPush, opts ...asynq.Option) error
	DistributeTaskSendMail(ctx context.Context, payload *PayloadSendMail, opts ...asynq.Option) error
	Close()
}

// RedisTaskDistributor enqueues tasks to Redis using asynq.Client.
type RedisTaskDistributor struct {
	client *asynq.Client
	logger *zap.SugaredLogger
}

// NewRedisTaskDistributor creates a distributor backed by the given Redis options.
func NewRedisTaskDistributor(redisOpt asynq.RedisConnOpt, logger *zap.SugaredLogger) *RedisTaskDistributor {
	return &RedisTaskDistributor{
		client: asynq.NewClient(redisOpt),
		logger: logger,
	}
}

func (d *RedisTaskDistributor) Close() {
	_ = d.client.Close()
}

// enqueue serialises the payload and calls client.EnqueueContext.
// The returned TaskInfo (ID, queue, deadline) is logged for traceability.
func (d *RedisTaskDistributor) enqueue(ctx context.Context, taskType string, payload []byte, opts []asynq.Option) error {
	task := asynq.NewTask(taskType, payload)
	info, err := d.client.EnqueueContext(ctx, task, opts...)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", taskType, err)
	}
	d.logger.Debugw("task enqueued",
		"type", info.Type,
		"id", info.ID,
		"queue", info.Queue,
		"max_retry", info.MaxRetry,
	)
	return nil
}
