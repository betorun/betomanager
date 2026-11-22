// Package queue : tâches de fond sur Redis (asynq). Aujourd'hui l'envoi des
// e-mails : l'API met en file, le worker envoie, avec reprises espacées ; une
// tâche épuisée reste archivée dans Redis (jamais perdue sans trace).
package queue

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// TaskDistributor : mise en file des tâches.
type TaskDistributor interface {
	DistributeTaskSendMail(ctx context.Context, payload *PayloadSendMail, opts ...asynq.Option) error
	Close()
}

// RedisTaskDistributor : file asynq sur le client Redis de l'application.
type RedisTaskDistributor struct {
	client *asynq.Client
	logger *zap.SugaredLogger
}

// NewRedisTaskDistributor : partage le client Redis (Close ne le ferme pas).
func NewRedisTaskDistributor(rdb redis.UniversalClient, logger *zap.SugaredLogger) *RedisTaskDistributor {
	return &RedisTaskDistributor{client: asynq.NewClientFromRedisClient(rdb), logger: logger}
}

func (d *RedisTaskDistributor) Close() { _ = d.client.Close() }

func (d *RedisTaskDistributor) enqueue(ctx context.Context, taskType string, payload []byte, opts []asynq.Option) error {
	info, err := d.client.EnqueueContext(ctx, asynq.NewTask(taskType, payload), opts...)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", taskType, err)
	}
	d.logger.Debugw("task enqueued", "type", info.Type, "id", info.ID, "queue", info.Queue, "max_retry", info.MaxRetry)
	return nil
}
