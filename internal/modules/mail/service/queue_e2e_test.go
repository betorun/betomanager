package service

// La file réelle (asynq sur Redis) : mise en file par QueueSender, livraison
// par le processeur. Ignoré sans MAIL_E2E_REDIS (ex. 127.0.0.1:56379) :
//
//	docker run -d --rm --name mail-e2e-redis -p 127.0.0.1:56379:6379 redis:7-alpine
//	MAIL_E2E_REDIS=127.0.0.1:56379 go test ./internal/modules/mail/service/ -run E2E -count=1

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
)

type chanSMTP struct {
	sent chan mail.Message
}

func (c *chanSMTP) Send(_ context.Context, m mail.Message) error { c.sent <- m; return nil }

func TestE2E_QueueDelivers(t *testing.T) {
	addr := os.Getenv("MAIL_E2E_REDIS")
	if addr == "" {
		t.Skip("MAIL_E2E_REDIS not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	require.NoError(t, rdb.FlushDB(context.Background()).Err())

	dist := queue.NewRedisTaskDistributor(rdb, log)
	defer dist.Close()
	smtp := &chanSMTP{sent: make(chan mail.Message, 1)}
	p := queue.NewRedisTaskProcessor(rdb, queue.ProcessorConfig{Concurrency: 2, ShutdownTimeout: time.Second}, log)
	p.RegisterHandlers(queue.Handlers{HandleSendMail: (&Deliverer{SMTP: smtp, Suppressions: newMem(), Logger: log}).HandleSendMail})
	require.NoError(t, p.Start())
	defer p.Shutdown()

	s := &QueueSender{Queue: dist, Suppressions: newMem(), Logger: log}
	require.NoError(t, s.Send(context.Background(), mail.Message{To: "awa@ex.sn", Subject: "Bienvenue", Text: "Bonjour",
		ReplyTo: "support@betorun.app", Attachments: []mail.Attachment{{Filename: "a.pdf", ContentType: "application/pdf", Data: []byte{0, 1, 2}}}}))

	select {
	case m := <-smtp.sent:
		assert.Equal(t, "awa@ex.sn", m.To)
		assert.Equal(t, "support@betorun.app", m.ReplyTo)
		assert.Equal(t, []byte{0, 1, 2}, m.Attachments[0].Data, "octets intacts après le passage par Redis")
	case <-time.After(15 * time.Second):
		t.Fatal("mail jamais livré par le worker")
	}
}
