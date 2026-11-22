package mail

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
)

// HandleSendMail is the asynq.HandlerFunc for "mail:send_email" tasks.
// If HTMLBody is pre-rendered it is used directly; otherwise the template is
// rendered via Mailer.GenerateHTML before sending.
func (w *MailWorker) HandleSendMail(ctx context.Context, task *asynq.Task) error {
	var p queue.PayloadSendMail
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return fmt.Errorf("HandleSendMail: unmarshal: %w", err)
	}

	subject, template, vars := localizeMail(ctx, p.To, p.Subject, p.Template, p.Variables)
	p.Subject = subject
	htmlBody := p.HTMLBody
	if htmlBody == "" {
		body, err := w.mailer.GenerateHTML(template, vars)
		if err != nil {
			// Template errors are non-retryable (same template will fail again).
			w.logger.Errorw("HandleSendMail: generate HTML failed, dropping",
				"template", p.Template, "error", err)
			return nil
		}
		htmlBody = body
	}

	if err := sendWithAttachments(w.mailer, []string{p.To}, p.Subject, htmlBody, p.Attachments); err != nil {
		if p.Ref != "" && isRecipientRejectedSMTPError(err) {
			w.logger.Errorw("HandleSendMail: recipient rejected, dropping", "to", p.To, "error", err)
			w.notifyDelivery(ctx, p.Ref, true)
			return nil
		}
		if isNonRetryableSMTPError(err) {
			w.logger.Errorw("HandleSendMail: non-retryable SMTP error, dropping",
				"to", p.To, "error", err)
			return nil
		}
		return fmt.Errorf("HandleSendMail: SMTP: %w", err)
	}
	w.logger.Infow("HandleSendMail: email sent", "to", p.To, "template", p.Template)
	w.notifyDelivery(ctx, p.Ref, false)
	return nil
}
