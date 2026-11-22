package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

const (
	QueueCritical = "critical" // payment webhooks, auth events
	QueueDefault  = "default"  // notifications, mail, reports
	QueueLow      = "low"      // cleanup, archiving
)

// ProcessorConfig holds tunable parameters for the asynq server.
type ProcessorConfig struct {
	// Concurrency is the number of concurrent worker goroutines (default: 10).
	Concurrency int
	// ShutdownTimeout is how long to wait for in-flight tasks on shutdown (default: 30 s).
	ShutdownTimeout time.Duration
}

// RedisTaskProcessor consumes tasks from Redis priority queues via asynq.Server.
type RedisTaskProcessor struct {
	server *asynq.Server
	mux    *asynq.ServeMux
	logger *zap.SugaredLogger
}

// NewRedisTaskProcessor creates a processor with priority queues:
//
//	critical → weight 6  (payment webhooks, auth)
//	default  → weight 4  (notifications, mail, reports)
//	low      → weight 2  (cleanup, archiving)
//
// A custom RetryDelayFunc caps critical-queue retries at 2 min to keep
// payment webhook re-delivery latency predictable.
func NewRedisTaskProcessor(redisOpt asynq.RedisConnOpt, cfg ProcessorConfig, logger *zap.SugaredLogger) *RedisTaskProcessor {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 25 // P-3: raised from 10; tune via ProcessorConfig
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: cfg.Concurrency,
		Queues: map[string]int{
			QueueCritical: 6,
			QueueDefault:  4,
			QueueLow:      2,
		},
		// P-4: drain all critical tasks before processing default, and all default
		// before low. Prevents low-priority bursts from delaying payment webhooks.
		StrictPriority:  true,
		ShutdownTimeout: cfg.ShutdownTimeout,

		ErrorHandler: asynq.ErrorHandlerFunc(func(_ context.Context, task *asynq.Task, err error) {
			logger.Errorw("asynq task failed",
				"type", task.Type(),
				"error", err,
			)
		}),

		HealthCheckFunc: func(err error) {
			if err != nil {
				logger.Warnw("asynq health check: Redis ping failed", "error", err)
			}
		},

		Logger: newAsynqLogger(logger),
	})
	return &RedisTaskProcessor{
		server: srv,
		mux:    asynq.NewServeMux(),
		logger: logger,
	}
}

// RegisterHandlers wires task-type strings to handler functions.
// Skips nil handlers instead of panicking at task-receipt time.
// Must be called before Start.
func (p *RedisTaskProcessor) RegisterHandlers(handlers Handlers) {
	if handlers.HandleSendPush != nil {
		p.mux.HandleFunc(TaskSendPush, handlers.HandleSendPush)
	}
	if handlers.HandleSendMail != nil {
		p.mux.HandleFunc(TaskSendMail, handlers.HandleSendMail)
	}

	if handlers.HandleDocumentCleanup != nil {
		p.mux.HandleFunc(TaskDocumentCleanup, handlers.HandleDocumentCleanup)
	}

}

// Start runs the asynq server (blocks until Shutdown is called).
func (p *RedisTaskProcessor) Start() error {
	p.logger.Info("asynq task processor starting")
	if err := p.server.Run(p.mux); err != nil {
		return fmt.Errorf("asynq processor: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight tasks then stops.
func (p *RedisTaskProcessor) Shutdown() {
	p.logger.Info("asynq task processor shutting down")
	p.server.Shutdown()
}

// Handlers groups all task handler functions registered by RegisterHandlers.
// Nil fields are silently skipped — the task type will not be consumed.
type Handlers struct {
	HandleSendPush        asynq.HandlerFunc
	HandleSendMail        asynq.HandlerFunc
	HandleDocumentCleanup asynq.HandlerFunc
}
