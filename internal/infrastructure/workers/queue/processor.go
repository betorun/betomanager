package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Files, par priorité (stricte) : critical d'abord, puis default, puis low.
const (
	QueueCritical = "critical"
	QueueDefault  = "default" // e-mails
	QueueLow      = "low"
)

// ProcessorConfig : réglages du serveur asynq.
type ProcessorConfig struct {
	// Concurrency : tâches traitées en parallèle (défaut 10).
	Concurrency int
	// ShutdownTimeout : attente des tâches en cours à l'arrêt (défaut 30 s).
	ShutdownTimeout time.Duration
}

// RedisTaskProcessor : consomme les files (worker).
type RedisTaskProcessor struct {
	server *asynq.Server
	mux    *asynq.ServeMux
	logger *zap.SugaredLogger
}

func NewRedisTaskProcessor(rdb redis.UniversalClient, cfg ProcessorConfig, logger *zap.SugaredLogger) *RedisTaskProcessor {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	srv := asynq.NewServerFromRedisClient(rdb, asynq.Config{
		Concurrency:     cfg.Concurrency,
		Queues:          map[string]int{QueueCritical: 6, QueueDefault: 4, QueueLow: 2},
		StrictPriority:  true,
		ShutdownTimeout: cfg.ShutdownTimeout,
		RetryDelayFunc: func(n int, err error, t *asynq.Task) time.Duration {
			if t.Type() == TaskSendMail {
				return MailRetryDelay(n)
			}
			return asynq.DefaultRetryDelayFunc(n, err, t)
		},
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
			retried, _ := asynq.GetRetryCount(ctx)
			maxRetry, _ := asynq.GetMaxRetry(ctx)
			logger.Warnw("background task failed", "type", task.Type(), "attempt", retried+1, "max_retry", maxRetry, "error", err)
		}),
		HealthCheckFunc: func(err error) {
			if err != nil {
				logger.Warnw("asynq health check: Redis ping failed", "error", err)
			}
		},
		Logger: newAsynqLogger(logger),
	})
	return &RedisTaskProcessor{server: srv, mux: asynq.NewServeMux(), logger: logger}
}

// Handlers : un champ par type de tâche (nil : non consommée).
type Handlers struct {
	HandleSendMail asynq.HandlerFunc
}

// RegisterHandlers : avant Start.
func (p *RedisTaskProcessor) RegisterHandlers(h Handlers) {
	if h.HandleSendMail != nil {
		p.mux.HandleFunc(TaskSendMail, h.HandleSendMail)
	}
}

// Start : démarre sans bloquer.
func (p *RedisTaskProcessor) Start() error {
	p.logger.Info("background task processor starting")
	if err := p.server.Start(p.mux); err != nil {
		return fmt.Errorf("asynq processor: %w", err)
	}
	return nil
}

// Shutdown : termine les tâches en cours puis s'arrête.
func (p *RedisTaskProcessor) Shutdown() {
	p.logger.Info("background task processor shutting down")
	p.server.Shutdown()
}
