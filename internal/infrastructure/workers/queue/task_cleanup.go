package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TaskDocumentCleanup = "document:cleanup"

// PayloadDocumentCleanup requests deletion of soft-deleted documents older than the cutoff.
type PayloadDocumentCleanup struct {
	SchoolID  string `json:"school_id,omitempty"` // empty = all schools
	BatchSize int    `json:"batch_size,omitempty"`
}

func (d *RedisTaskDistributor) DistributeTaskDocumentCleanup(ctx context.Context, payload *PayloadDocumentCleanup, opts ...asynq.Option) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal PayloadDocumentCleanup: %w", err)
	}
	defaults := []asynq.Option{
		asynq.Queue(QueueLow),
		asynq.MaxRetry(1),
		asynq.Timeout(5 * 60 * 1e9), // 5 min max
		// P-2: dedup — only one cleanup task at a time across all instances.
		asynq.TaskID("document-cleanup"),
	}
	return d.enqueue(ctx, TaskDocumentCleanup, b, append(defaults, opts...))
}
