package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
)

type memSuppressions struct {
	mu   sync.Mutex
	rows map[string]domain.Suppression
}

func newMem() *memSuppressions { return &memSuppressions{rows: map[string]domain.Suppression{}} }

func (m *memSuppressions) IsSuppressed(_ context.Context, e string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[domain.NormalizeEmail(e)]
	return ok, nil
}
func (m *memSuppressions) Suppress(_ context.Context, s domain.Suppression) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[s.Email]; !ok {
		m.rows[s.Email] = s
	}
	return nil
}
func (m *memSuppressions) List(context.Context, string, int) ([]domain.Suppression, error) { return nil, nil }
func (m *memSuppressions) Remove(_ context.Context, e string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[e]
	delete(m.rows, e)
	return ok, nil
}

type fakeQueue struct{ got []queue.PayloadSendMail }

func (f *fakeQueue) DistributeTaskSendMail(_ context.Context, p *queue.PayloadSendMail, _ ...asynq.Option) error {
	f.got = append(f.got, *p)
	return nil
}

type fakeSMTP struct {
	err  error
	sent []mail.Message
}

func (f *fakeSMTP) Send(_ context.Context, m mail.Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

var log = zap.NewNop().Sugar()

func TestQueueSender_SkipsSuppressed(t *testing.T) {
	q, sup := &fakeQueue{}, newMem()
	s := &QueueSender{Queue: q, Suppressions: sup, Logger: log}
	require.NoError(t, sup.Suppress(context.Background(), domain.NewSuppression("gone@ex.sn", domain.ReasonBounce, domain.SourceSendGrid, "")))

	require.NoError(t, s.Send(context.Background(), mail.Message{To: "Gone@Ex.sn", Subject: "x"}))
	require.NoError(t, s.Send(context.Background(), mail.Message{To: "awa@ex.sn", Subject: "Votre facture",
		Attachments: []mail.Attachment{{Filename: "f.pdf", Data: []byte("pdf")}}}))
	require.Len(t, q.got, 1)
	assert.Equal(t, "awa@ex.sn", q.got[0].Message.To)
	assert.Equal(t, []byte("pdf"), q.got[0].Message.Attachments[0].Data, "pièce jointe gardée dans la file")
}

func task(t *testing.T, m mail.Message) *asynq.Task {
	b, err := json.Marshal(queue.PayloadSendMail{Message: m})
	require.NoError(t, err)
	return asynq.NewTask(queue.TaskSendMail, b)
}

func TestDeliverer_Outcomes(t *testing.T) {
	ctx := context.Background()
	msg := mail.Message{To: "awa@ex.sn", Subject: "Bienvenue"}

	smtp, sup := &fakeSMTP{}, newMem()
	d := &Deliverer{SMTP: smtp, Suppressions: sup, Logger: log}
	require.NoError(t, d.HandleSendMail(ctx, task(t, msg)))
	assert.Len(t, smtp.sent, 1, "envoyé")

	smtp.err = errors.New("dial tcp: connection refused")
	err := d.HandleSendMail(ctx, task(t, msg))
	require.Error(t, err)
	assert.NotErrorIs(t, err, asynq.SkipRetry, "panne passagère : réessayé")

	smtp.err = fmt.Errorf("mail: rcpt: %w", &textproto.Error{Code: 550, Msg: "5.1.1 user unknown"})
	require.NoError(t, d.HandleSendMail(ctx, task(t, msg)), "adresse refusée : fin de la tâche")
	ok, _ := sup.IsSuppressed(ctx, "awa@ex.sn")
	assert.True(t, ok, "adresse mise de côté")
	smtp.err = nil
	require.NoError(t, d.HandleSendMail(ctx, task(t, msg)))
	assert.Len(t, smtp.sent, 1, "plus rien n'est envoyé à une adresse mise de côté")

	smtp.err = fmt.Errorf("mail: data: %w", &textproto.Error{Code: 554, Msg: "5.6.0 content rejected"})
	err = d.HandleSendMail(ctx, task(t, mail.Message{To: "bob@ex.sn"}))
	assert.ErrorIs(t, err, asynq.SkipRetry, "message refusé : archivé sans reprise")

	assert.ErrorIs(t, d.HandleSendMail(ctx, asynq.NewTask(queue.TaskSendMail, []byte("{bad"))), asynq.SkipRetry)
}

func TestSendGridWebhook(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	pub, err := ParseSendGridPublicKey(base64.StdEncoding.EncodeToString(der))
	require.NoError(t, err)

	now := time.Unix(1_800_000_000, 0)
	body := []byte(`[{"email":"Gone@ex.sn","event":"bounce","type":"bounce","reason":"550 user unknown"},
		{"email":"full@ex.sn","event":"bounce","type":"blocked"},
		{"email":"angry@ex.sn","event":"spamreport"},
		{"email":"ok@ex.sn","event":"delivered"}]`)
	sign := func(ts string, b []byte) string {
		h := sha256.Sum256(append([]byte(ts), b...))
		sig, err := ecdsa.SignASN1(rand.Reader, key, h[:])
		require.NoError(t, err)
		return base64.StdEncoding.EncodeToString(sig)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	sup := newMem()
	w := &SendGridWebhook{Key: pub, Suppressions: sup}

	_, err = w.Handle(context.Background(), sign(ts, body), ts, append(body, ' '), now)
	assert.ErrorIs(t, err, ErrBadSignature, "corps modifié")
	old := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	_, err = w.Handle(context.Background(), sign(old, body), old, body, now)
	assert.ErrorIs(t, err, ErrBadSignature, "lot rejoué une heure plus tard")

	n, err := w.Handle(context.Background(), sign(ts, body), ts, body, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, domain.ReasonBounce, sup.rows["gone@ex.sn"].Reason)
	assert.Equal(t, domain.ReasonComplaint, sup.rows["angry@ex.sn"].Reason)
	assert.NotContains(t, sup.rows, "full@ex.sn", "blocked : l'adresse reste valable")
}
