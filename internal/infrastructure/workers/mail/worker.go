package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"go.uber.org/zap"
)

const (
	defaultMailWorkerCount       = 8
	defaultMailMaxOutstanding    = 64
	defaultMailProcessingTimeout = 15 * time.Second
)

// Mailer generates HTML from a template and sends it via SMTP.
type Mailer interface {
	GenerateHTML(templateName string, data map[string]string) (string, error)
	SendEmail(to []string, subject string, htmlBody string) error
}

// MailWorker consumes mail events from Pub/Sub and sends them via SMTP.
// It implements common.PeriodicWorker: Start(ctx) blocks until ctx is cancelled.
type MailWorker struct {
	client *pubsub.Client
	mailer Mailer
	subID  string
	logger *zap.SugaredLogger

	onDelivery DeliveryHook
}

// DeliveryHook est appelé pour un mail suivi (Ref non vide) : accepté par le
// serveur SMTP, ou adresse refusée définitivement (rejected).
type DeliveryHook func(ctx context.Context, ref string, rejected bool)

// AttachDeliveryHook câble le suivi d'envoi.
func (w *MailWorker) AttachDeliveryHook(hook DeliveryHook) {
	w.onDelivery = hook
}

func (w *MailWorker) notifyDelivery(ctx context.Context, ref string, rejected bool) {
	if ref != "" && w.onDelivery != nil {
		w.onDelivery(ctx, ref, rejected)
	}
}

func NewMailWorker(client *pubsub.Client, mailer Mailer, subID string, logger *zap.SugaredLogger) *MailWorker {
	return &MailWorker{client: client, mailer: mailer, subID: subID, logger: logger}
}

// Start subscribes to the mail topic and blocks until ctx is cancelled.
func (w *MailWorker) Start(ctx context.Context) {
	w.logger.Infow("Starting Mail Worker", "subscription", w.subID)
	sub := w.client.Subscription(w.subID)
	sub.ReceiveSettings.NumGoroutines = defaultMailWorkerCount
	sub.ReceiveSettings.MaxOutstandingMessages = defaultMailMaxOutstanding

	err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		w.processMessage(ctx, msg)
	})
	if err != nil {
		w.logger.Errorw("Mail worker error", "error", err)
	}
}

func (w *MailWorker) processMessage(ctx context.Context, msg *pubsub.Message) {
	w.logger.Debugw("Mail event received", "bytes", len(msg.Data), "attempt", deliveryAttempt(msg))

	var event mailSvc.MailEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		w.logger.Error("Invalid mail event format")
		msg.Ack()
		return
	}
	applicationID := strings.TrimSpace(event.Variables["application_id"])
	w.logger.Debugw("Mail event decoded", "to", event.To, "template", event.Template, "subject", event.Subject, "application_id", applicationID)

	processCtx, cancel := context.WithTimeout(ctx, defaultMailProcessingTimeout)
	defer cancel()

	subject, template, vars := localizeMail(processCtx, event.To, event.Subject, event.Template, event.Variables)
	event.Subject = subject
	htmlBody, err := w.mailer.GenerateHTML(template, vars)
	if err != nil {
		w.logger.Errorw("Failed to generate email template", "template", event.Template, "error", err, "application_id", applicationID)
		msg.Ack()
		return
	}

	select {
	case <-processCtx.Done():
		w.logger.Errorw("Mail processing timeout before SMTP send", "to", event.To, "template", event.Template, "application_id", applicationID)
		msg.Nack()
		return
	default:
	}

	err = sendWithAttachments(w.mailer, []string{event.To}, event.Subject, htmlBody, event.Attachments)
	if err != nil {
		if event.Ref != "" && isRecipientRejectedSMTPError(err) {
			w.logger.Errorw("Recipient rejected by SMTP server, dropping message", "error", err, "to", event.To, "template", event.Template)
			w.notifyDelivery(processCtx, event.Ref, true)
			msg.Ack()
			return
		}
		if isNonRetryableSMTPError(err) {
			w.logger.Errorw("Non-retryable SMTP error, dropping message", "error", err, "to", event.To, "template", event.Template, "application_id", applicationID)
			msg.Ack()
			return
		}
		attempt := deliveryAttempt(msg)
		w.logger.Errorw("Failed to send email via SMTP", "error", err, "attempt", attempt, "to", event.To, "template", event.Template, "application_id", applicationID)
		// Pas d'Ack « au bout de N essais » : le mail serait perdu sans trace.
		// La politique dead-letter de la souscription (5 tentatives, cf.
		// common.MaxRetryAttempts) arrête la boucle et garde le message.
		select {
		case <-time.After(backoffForAttempt(attempt)):
		case <-processCtx.Done():
			w.logger.Errorw("Mail retry backoff canceled by context", "to", event.To, "template", event.Template, "attempt", attempt, "application_id", applicationID)
		}
		msg.Nack()
		return
	}

	w.logger.Infow("Email sent successfully", "to", event.To, "template", event.Template, "application_id", applicationID)
	w.notifyDelivery(processCtx, event.Ref, false)
	msg.Ack()
}

func deliveryAttempt(msg *pubsub.Message) int {
	if msg == nil || msg.DeliveryAttempt == nil || *msg.DeliveryAttempt <= 0 {
		return 1
	}
	return int(*msg.DeliveryAttempt)
}

func backoffForAttempt(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 1 * time.Second
	case 2:
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

func isNonRetryableSMTPError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	needles := []string{
		"535",
		"5.7.8",
		"badcredentials",
		"username and password not accepted",
		"454 4.7.0",
		"too many login attempts",
		"too many failed login attempts",
	}

	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}

	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

var recipientRejectedCode = regexp.MustCompile(`\b55[013]\b`)

// isRecipientRejectedSMTPError : le serveur refuse cette adresse, définitivement.
// Les codes 550, 551 et 553 le disent quelle que soit leur formulation, sauf quand
// la cause est ailleurs : expéditeur, réputation d'envoi, volume. Ces refus-là
// restent des échecs ordinaires, réessayés puis signalés sur le dossier.
func isRecipientRejectedSMTPError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !recipientRejectedCode.MatchString(msg) {
		return false
	}
	for _, n := range []string{
		"5.7.", "5.4.", "5.1.7", "5.1.8", "5.3.",
		"sender", "from address", "relay", "reputation", "blocked", "blacklist", "blocklist",
		"policy", "spam", "limit", "quota", "rate", "unauthenticated", "authentication",
	} {
		if strings.Contains(msg, n) {
			return false
		}
	}
	return true
}

func sendWithAttachments(m Mailer, to []string, subject, body string, attachments []mailcontract.Attachment) error {
	if len(attachments) == 0 {
		return m.SendEmail(to, subject, body)
	}
	sender, ok := m.(interface {
		SendEmailWithAttachments([]string, string, string, []mailcontract.Attachment) error
	})
	if !ok {
		return fmt.Errorf("mail sender does not support attachments")
	}
	return sender.SendEmailWithAttachments(to, subject, body, attachments)
}
