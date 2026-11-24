package services

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/auth/social"
	keycloakadmin "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
)

// kcByEmail : annuaire en mémoire, recherche par adresse exacte.
type kcByEmail struct {
	keycloakadmin.KeycloakClient
	users map[string]*keycloakadmin.UserProfile
}

func (k kcByEmail) FindUserByEmail(_ context.Context, email string) (*keycloakadmin.UserProfile, error) {
	if u, ok := k.users[email]; ok {
		return u, nil
	}
	return nil, keycloakadmin.ErrUserNotFound
}

func TestSocialAccount(t *testing.T) {
	kc := kcByEmail{users: map[string]*keycloakadmin.UserProfile{
		"awa@pro.sn":      {ID: "kc-pro", Email: "awa@pro.sn", Enabled: true, EmailVerified: true},
		"awa@perso.sn":    {ID: "kc-perso", Email: "awa@perso.sn", Enabled: true, EmailVerified: true},
		"jamais@ex.sn":    {ID: "kc-unverified", Email: "jamais@ex.sn", Enabled: true},
		"desactive@ex.sn": {ID: "kc-off", Email: "desactive@ex.sn", EmailVerified: true},
	}}
	s := &service{keycloak: kc}
	ctx := context.Background()

	u, email, err := s.socialAccount(ctx, &social.Profile{Email: "awa@pro.sn", OtherEmails: []string{"awa@perso.sn"}})
	require.NoError(t, err)
	assert.Equal(t, "kc-pro", u.ID, "l'adresse principale passe d'abord")
	assert.Equal(t, "awa@pro.sn", email)

	u, email, err = s.socialAccount(ctx, &social.Profile{Email: "nouvelle@gh.sn", OtherEmails: []string{"inconnue@ex.sn", "awa@perso.sn"}})
	require.NoError(t, err)
	assert.Equal(t, "kc-perso", u.ID, "compte retrouvé par une autre adresse vérifiée")
	assert.Equal(t, "awa@perso.sn", email, "le lien porte l'adresse du compte retrouvé")

	_, email, err = s.socialAccount(ctx, &social.Profile{Email: "nouvelle@gh.sn", OtherEmails: []string{"jamais@ex.sn", "desactive@ex.sn"}})
	assert.ErrorIs(t, err, keycloakadmin.ErrUserNotFound, "jamais un compte non confirmé ou désactivé par une autre adresse")
	assert.Equal(t, "nouvelle@gh.sn", email, "nouveau compte sur l'adresse principale")

	u, _, err = s.socialAccount(ctx, &social.Profile{Email: "jamais@ex.sn"})
	require.NoError(t, err)
	assert.Equal(t, "kc-unverified", u.ID, "adresse principale : comportement inchangé (compte sécurisé ensuite)")
}
