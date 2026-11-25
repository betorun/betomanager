package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/core/country"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	authrepos "github.com/thekrauss/kubemanager/internal/modules/auth/repository"
	keycloakadmin "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
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

// kcSignup : annuaire en mémoire (création, lecture par identifiant).
type kcSignup struct {
	keycloakadmin.KeycloakClient
	created []keycloakadmin.CreateUserInput
	byID    map[string]*keycloakadmin.UserProfile
	reads   int
	readErr error
}

var errStopAfterCreate = errors.New("stop after create")

func (k *kcSignup) FindUserByEmail(context.Context, string) (*keycloakadmin.UserProfile, error) {
	return nil, keycloakadmin.ErrUserNotFound
}

// CreateUser : retient la demande puis arrête l'inscription (l'envoi du lien
// de confirmation demanderait une base).
func (k *kcSignup) CreateUser(_ context.Context, in keycloakadmin.CreateUserInput) (*keycloakadmin.UserProfile, error) {
	k.created = append(k.created, in)
	return nil, errStopAfterCreate
}

func (k *kcSignup) GetUserByID(_ context.Context, id string) (*keycloakadmin.UserProfile, error) {
	k.reads++
	if k.readErr != nil {
		return nil, k.readErr
	}
	if u, ok := k.byID[id]; ok {
		return u, nil
	}
	return nil, keycloakadmin.ErrUserNotFound
}

// Pays donné à l'inscription : envoyé à l'annuaire (attribut « country »),
// aucun profil local créé avant la confirmation de l'adresse.
func TestRegister_CountryKeptInDirectory(t *testing.T) {
	kc := &kcSignup{}
	users := &signupUsers{byKC: map[string]*authdomain.User{}}
	s := NewService(Deps{Keycloak: kc, Users: users, Mailer: noMail{}, EmailTokens: &auth.EmailTokens{}, FrontendBaseURL: "https://console.test"})
	for _, c := range []string{" ga ", ""} {
		_, err := s.RegisterAccount(context.Background(), identitytypes.RegisterRequest{
			Email: "awa@betorun.test", Password: "un mot de passe solide 42", FullName: "Awa", Country: c}, Client{IP: "1.2.3.4"})
		assert.ErrorIs(t, err, errStopAfterCreate, c)
	}
	require.Len(t, kc.created, 2)
	assert.Equal(t, "GA", kc.created[0].Country, "code normalisé")
	assert.Empty(t, kc.created[1].Country, "pays facultatif")
	assert.Empty(t, users.byKC, "pas de profil local à l'inscription")
}

// Première connexion : le profil local reprend le pays gardé dans l'annuaire
// (l'organisation personnelle le lira à sa création) ; au mieux, un échec de
// lecture ou un code invalide donne un pays vide, jamais une erreur.
func TestLocalUser_SignupCountry(t *testing.T) {
	ctx := context.Background()
	ga, xx, gone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	kc := &kcSignup{byID: map[string]*keycloakadmin.UserProfile{
		ga: {ID: ga, Country: "ga"},
		xx: {ID: xx, Country: "XX"},
	}}
	users := &signupUsers{byKC: map[string]*authdomain.User{}}
	s := NewService(Deps{Keycloak: kc, Users: users}).(*service)

	u, err := s.localUser(ctx, ga, "awa@betorun.test", "Awa")
	require.NoError(t, err)
	assert.Equal(t, "GA", u.Country)
	assert.Equal(t, ga, u.ID.String(), "identifiant local = sub")
	assert.True(t, u.IsVerified, "créé à la connexion, adresse confirmée")

	u, err = s.localUser(ctx, xx, "x@betorun.test", "X")
	require.NoError(t, err)
	assert.Empty(t, u.Country, "code invalide ignoré")

	u, err = s.localUser(ctx, gone, "g@betorun.test", "G")
	require.NoError(t, err, "annuaire sans ce compte : profil créé quand même")
	assert.Empty(t, u.Country)

	// Profil existant : rendu tel quel, l'annuaire n'est pas relu.
	reads := kc.reads
	kc.byID[ga].Country = "CG"
	u, err = s.localUser(ctx, ga, "awa@betorun.test", "Awa")
	require.NoError(t, err)
	assert.Equal(t, "GA", u.Country)
	assert.Equal(t, reads, kc.reads)

	// Annuaire injoignable : pays vide, connexion possible.
	kc.readErr = errors.New("keycloak down")
	u, err = s.localUser(ctx, uuid.NewString(), "d@betorun.test", "D")
	require.NoError(t, err)
	assert.Empty(t, u.Country)
}
