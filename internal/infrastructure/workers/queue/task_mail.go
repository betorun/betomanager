package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TaskSendMail = "mail:send_email"

// PayloadSendMail holds everything needed to render a mail template and send via SMTP.
// Provide either (Template + Variables) or a pre-rendered HTMLBody.
type PayloadSendMail struct {
	Attachments []mailcontract.Attachment `json:"attachments,omitempty"`
	To          string                    `json:"to"`
	Subject     string                    `json:"subject"`
	Template    string                    `json:"template,omitempty"`
	Variables   map[string]string         `json:"variables,omitempty"`
	HTMLBody    string                    `json:"html_body,omitempty"` // pre-rendered, skips template
	Ref         string                    `json:"ref,omitempty"`       // renvoyée au crochet de livraison
}

func (d *RedisTaskDistributor) DistributeTaskSendMail(ctx context.Context, payload *PayloadSendMail, opts ...asynq.Option) error {
	if payload.To == "" {
		return fmt.Errorf("PayloadSendMail: To is required")
	}
	if payload.Template == "" && payload.HTMLBody == "" {
		return fmt.Errorf("PayloadSendMail: Template or HTMLBody is required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal PayloadSendMail: %w", err)
	}
	defaults := []asynq.Option{
		asynq.Queue(QueueDefault),
		asynq.MaxRetry(3),
		asynq.Timeout(2 * 60 * 1e9), // 2 min max for SMTP
	}
	return d.enqueue(ctx, TaskSendMail, b, append(defaults, opts...))
}
