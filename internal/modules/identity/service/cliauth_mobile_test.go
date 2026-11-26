package services

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/auth"
	keycloakadmin "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
	identitytypes "github.com/thekrauss/kubemanager/internal/modules/identity/types"
)

// memState : StateStore en mémoire.
type memState map[string]string

func (m memState) SetValue(_ context.Context, k, v string, _ time.Duration) error {
	m[k] = v
	return nil
}
func (m memState) GetValue(_ context.Context, k string) (string, error) { return m[k], nil }
func (m memState) DeleteValue(_ context.Context, k string) error {
	delete(m, k)
	return nil
}

const (
	appLink     = "https://admin.bisocloud.com/mobile/callback"
	testPKCE    = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	cliLoopback = "http://127.0.0.1:53682/callback"
)

func mobileService(state memState) *service {
	return &service{
		keycloak:        struct{ keycloakadmin.KeycloakClient }{},
		sessions:        &auth.Sessions{},
		factors:         &auth.Factors{},
		stateStore:      state,
		frontendBaseURL: "https://console.bisocloud.com",
		platformURL:     "https://admin.bisocloud.com/",
		mobileRedirects: redirectSet([]string{" " + appLink + " ", ""}),
	}
}

func TestCheckRedirect(t *testing.T) {
	s := mobileService(memState{})
	mobile, err := s.checkRedirect(cliLoopback, testPKCE)
	require.NoError(t, err)
	assert.False(t, mobile, "boucle locale : la CLI")

	mobile, err = s.checkRedirect(appLink, testPKCE)
	require.NoError(t, err)
	assert.True(t, mobile, "adresse exacte de la configuration : l'application mobile")

	for _, bad := range []string{
		appLink + "?x=1",
		appLink + "/",
		"https://evil.example/mobile/callback",
		"com.bisocloud.backoffice://callback",
		"",
	} {
		_, err := s.checkRedirect(bad, testPKCE)
		assert.ErrorIs(t, err, ErrCLIRedirectURI, bad)
	}

	_, err = s.checkRedirect(appLink, "court")
	assert.ErrorIs(t, err, ErrInvalidPKCE)

	closed := mobileService(memState{})
	closed.mobileRedirects = redirectSet(nil)
	_, err = closed.checkRedirect(appLink, testPKCE)
	assert.ErrorIs(t, err, ErrCLIRedirectURI, "sans configuration, la connexion mobile est fermée")
}

func TestCLIAuthorizeURLMobilePage(t *testing.T) {
	s := mobileService(memState{})
	ctx := context.Background()

	res, err := s.CLIAuthorizeURL(ctx, identitytypes.CLIAuthorizeQuery{RedirectURI: appLink, CodeChallenge: testPKCE, State: "s1"})
	require.NoError(t, err)
	u, err := url.Parse(res.URL)
	require.NoError(t, err)
	assert.Equal(t, "admin.bisocloud.com", u.Host, "page sur la console plateforme")
	assert.Equal(t, "/mobile/authorize", u.Path)
	assert.Equal(t, appLink, u.Query().Get("redirect_uri"))

	res, err = s.CLIAuthorizeURL(ctx, identitytypes.CLIAuthorizeQuery{RedirectURI: cliLoopback, CodeChallenge: testPKCE, State: "s1"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.URL, "https://console.bisocloud.com/cli/authorize?"), "la CLI ne change pas")
}

func TestExchangeMobileCodeRefusedWhenClosed(t *testing.T) {
	state := memState{}
	s := mobileService(state)
	ctx := context.Background()
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

	put := func(code string, c cliCode) {
		raw, _ := json.Marshal(c)
		state["auth:cli:code:"+digest(code)] = string(raw)
	}

	// Adresse retirée de la configuration entre l'autorisation et l'échange.
	put("c1", cliCode{UserID: uuid.New(), MFA: true, Challenge: testPKCE, RedirectURI: appLink, Mobile: true})
	s.mobileRedirects = redirectSet(nil)
	_, err := s.ExchangeCLICode(ctx, identitytypes.CLITokenRequest{Code: "c1", CodeVerifier: verifier, RedirectURI: appLink}, Client{})
	assert.ErrorIs(t, err, ErrCLICodeInvalid)
	assert.Empty(t, state, "code à usage unique, même refusé")

	// Jamais de session mobile sans second facteur.
	s.mobileRedirects = redirectSet([]string{appLink})
	put("c2", cliCode{UserID: uuid.New(), MFA: false, Challenge: testPKCE, RedirectURI: appLink, Mobile: true})
	_, err = s.ExchangeCLICode(ctx, identitytypes.CLITokenRequest{Code: "c2", CodeVerifier: verifier, RedirectURI: appLink}, Client{})
	assert.ErrorIs(t, err, ErrCLICodeInvalid)
}

func TestMobileSessionLabel(t *testing.T) {
	_, label := auth.DescribeDevice(auth.KindMobile, "Dart/3.10 (dart:io) android")
	assert.True(t, strings.HasPrefix(label, "App back-office"), label)
}
