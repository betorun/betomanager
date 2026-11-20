package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	authrepos "github.com/thekrauss/kubemanager/internal/modules/auth/repository"
)

type noticeUsers struct {
	authrepos.AuthRepository
	user *authdomain.User
}

func (u noticeUsers) GetUserByID(context.Context, uuid.UUID) (*authdomain.User, error) {
	return u.user, nil
}

type noticeMail struct{ sent []mail.Message }

func (m *noticeMail) Send(_ context.Context, msg mail.Message) error {
	m.sent = append(m.sent, msg)
	return nil
}

type noticeSpawner struct{ names []string }

func (s *noticeSpawner) Go(name string, fn func(context.Context)) {
	s.names = append(s.names, name)
	fn(context.Background())
}

// L'e-mail de sécurité part par le Spawner de l'application : l'arrêt
// l'attend au lieu de perdre l'avertissement (mot de passe changé, MFA…).
func TestSecurityMail_UsesSpawner(t *testing.T) {
	m := &noticeMail{}
	sp := &noticeSpawner{}
	s := &service{mailer: m, frontendBaseURL: "https://console.test", spawner: sp,
		users: noticeUsers{user: &authdomain.User{Email: "awa@x.test", FullName: "Awa", Locale: "fr"}}}
	s.securityMail(context.Background(), uuid.New(), emailPasswordChanged, "https://console.test/settings",
		map[string]string{"At": "2026-11-20T08:00:00Z", "IP": "41.82.1.2"})
	require.Len(t, m.sent, 1)
	assert.Equal(t, "awa@x.test", m.sent[0].To)
	assert.Equal(t, []string{"security-mail"}, sp.names)
}
