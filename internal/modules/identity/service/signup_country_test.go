package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/core/country"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	authrepos "github.com/thekrauss/kubemanager/internal/modules/auth/repository"
	identitytypes "github.com/thekrauss/kubemanager/internal/modules/identity/types"
)

// signupUsers : profils locaux en mémoire.
type signupUsers struct {
	authrepos.AuthRepository
	byKC map[string]*authdomain.User
}

func (u *signupUsers) FindByKeycloakUserID(_ context.Context, id string) (*authdomain.User, error) {
	return u.byKC[id], nil
}

func (u *signupUsers) CreateUser(_ context.Context, user *authdomain.User) error {
	u.byKC[user.KeycloakUserID] = user
	return nil
}

func (u *signupUsers) UpdateUser(_ context.Context, user *authdomain.User) error {
	u.byKC[user.KeycloakUserID] = user
	return nil
}

type noMail struct{}

func (noMail) Send(context.Context, mail.Message) error { return nil }

// Pays inconnu : refusé avant toute création de compte.
func TestRegister_InvalidCountry(t *testing.T) {
	s := NewService(Deps{Keycloak: kcProfile{}, Users: &signupUsers{byKC: map[string]*authdomain.User{}},
		Mailer: noMail{}, EmailTokens: &auth.EmailTokens{}, FrontendBaseURL: "https://console.test"})
	for _, c := range []string{"XX", "GAB", "uk"} {
		_, err := s.RegisterAccount(context.Background(), identitytypes.RegisterRequest{
			Email: "awa@betorun.test", Password: "un mot de passe solide 42", FullName: "Awa", Country: c}, Client{IP: "1.2.3.4"})
		assert.ErrorIs(t, err, country.ErrInvalid, c)
	}
}

// Pays donné à l'inscription : gardé sur le profil local (l'organisation
// personnelle le reprend à sa création) ; un pays déjà connu ne change pas.
func TestRegister_RememberCountry(t *testing.T) {
	users := &signupUsers{byKC: map[string]*authdomain.User{}}
	s := NewService(Deps{Users: users}).(*service)
	sub := uuid.NewString()

	s.rememberCountry(context.Background(), sub, "awa@betorun.test", "Awa", "")
	assert.Empty(t, users.byKC, "sans pays : profil créé à la première connexion, comme avant")

	s.rememberCountry(context.Background(), sub, "awa@betorun.test", "Awa", "GA")
	u := users.byKC[sub]
	require.NotNil(t, u)
	assert.Equal(t, "GA", u.Country)
	assert.Equal(t, sub, u.ID.String(), "identifiant local = sub, comme à la première connexion")
	assert.False(t, u.IsVerified, "adresse pas encore confirmée")

	s.rememberCountry(context.Background(), sub, "awa@betorun.test", "Awa", "CG")
	assert.Equal(t, "GA", users.byKC[sub].Country)
}
