package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mailcontract "github.com/thekrauss/betorun/betorunapi/internal/modules/mail/contract"

	"go.uber.org/zap"
)

// ErrNotConfigured : aucune file de mails n'est câblée, le mail ne partira pas.
var ErrNotConfigured = errors.New("mail publisher not configured")

type MailEvent struct {
	Attachments []mailcontract.Attachment `json:"attachments,omitempty"`
	To          string                    `json:"to"`
	Subject     string                    `json:"subject"`
	Template    string                    `json:"template"`
	Variables   map[string]string         `json:"variables"`
	// Ref : renvoyée au crochet de livraison du worker, pour l'appelant qui suit l'envoi.
	Ref string `json:"ref,omitempty"`
}

type MailPublisher struct {
	pubsub EventPublisher
	topic  string
	logger *zap.SugaredLogger
}

type EventPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

func NewMailPublisher(ps EventPublisher, topic string, logger *zap.SugaredLogger) *MailPublisher {
	return &MailPublisher{pubsub: ps, topic: topic, logger: logger}
}

func (p *MailPublisher) SendAsync(to, subject, template string, vars map[string]string) {
	err := p.SendTracked(to, subject, template, vars, "")
	if err != nil && !errors.Is(err, ErrNotConfigured) && p.logger != nil {
		p.logger.Errorw("mail publish failed", "error", err, "to", to, "template", template)
	}
}

// SendTracked met le mail en file et dit si cela a réussi. L'envoi SMTP reste
// asynchrone : ref revient au crochet de livraison du worker une fois le mail
// accepté ou refusé par le serveur.
func (p *MailPublisher) SendTracked(to, subject, template string, vars map[string]string, ref string) error {
	if p == nil || p.pubsub == nil || strings.TrimSpace(p.topic) == "" {
		return ErrNotConfigured
	}

	event := MailEvent{
		To:        to,
		Subject:   subject,
		Template:  template,
		Variables: vars,
		Ref:       ref,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("mail publish marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return p.pubsub.Publish(ctx, p.topic, data)
}
