package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/iam"
)

// fakeIdP : fournisseur OpenID Connect minimal (découverte, JWKS, jeton).
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any // claims de l'id_token suivant
	codes  map[string]string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeIdP{key: k, codes: map[string]string{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		e := big.NewInt(int64(k.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "s3cret" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(f.claims))
		tok.Header["kid"] = "k1"
		signed, _ := tok.SignedString(k)
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed, "access_token": "x", "token_type": "Bearer"})
	})
	return f
}

func (f *fakeIdP) next(nonce string, extra map[string]any) {
	now := time.Now()
	f.claims = map[string]any{"iss": f.srv.URL, "aud": "betorun", "sub": "u-1", "email": "awa@exemple.sn",
		"email_verified": true, "name": "Awa", "nonce": nonce, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	for k, v := range extra {
		if v == nil {
			delete(f.claims, k)
		} else {
			f.claims[k] = v
		}
	}
}

func TestOIDC_ExchangeVerifiesIDToken(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	o := NewOIDC(true)
	c := Client{Issuer: idp.srv.URL, ClientID: "betorun", ClientSecret: "s3cret"}

	u, err := o.AuthCodeURL(ctx, c, "st", "ch", "n1", "http://localhost:3000/auth/sso/callback", "awa@exemple.sn")
	require.NoError(t, err)
	parsed, _ := url.Parse(u)
	assert.Equal(t, idp.srv.URL+"/auth", parsed.Scheme+"://"+parsed.Host+parsed.Path)
	assert.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))
	assert.Equal(t, "awa@exemple.sn", parsed.Query().Get("login_hint"))

	idp.next("n1", nil)
	id, err := o.Exchange(ctx, c, "code", "verifier", "n1", "http://localhost:3000/auth/sso/callback")
	require.NoError(t, err)
	assert.Equal(t, "u-1", id.Subject)
	assert.Equal(t, "awa@exemple.sn", id.Email)

	for name, extra := range map[string]map[string]any{
		"nonce rejoué":       {"nonce": "autre"},
		"autre audience":     {"aud": "ailleurs"},
		"autre émetteur":     {"iss": "https://pirate.example"},
		"expiré":             {"exp": time.Now().Add(-time.Hour).Unix()},
		"sans sujet":         {"sub": ""},
		"sans date d'expiry": {"exp": nil},
	} {
		idp.next("n1", extra)
		_, err := o.Exchange(ctx, c, "code", "verifier", "n1", "x")
		assert.ErrorIs(t, err, ErrProviderRefused, name)
	}
	_, err = o.Exchange(ctx, Client{Issuer: c.Issuer, ClientID: "betorun", ClientSecret: "faux"}, "code", "v", "n1", "x")
	assert.ErrorIs(t, err, ErrProviderRefused, "secret refusé par le fournisseur")
}

func TestOIDC_IssuerRules(t *testing.T) {
	strict := NewOIDC(false)
	for _, bad := range []string{"http://idp.example", "https://idp.example?x=1", "https://u:p@idp.example", "ftp://x", ""} {
		_, err := strict.NormalizeIssuer(bad)
		assert.ErrorIs(t, err, ErrInvalidIssuer, bad)
	}
	got, err := strict.NormalizeIssuer(" https://idp.example/realms/a/ ")
	require.NoError(t, err)
	assert.Equal(t, "https://idp.example/realms/a", got)
	_, err = NewOIDC(true).NormalizeIssuer("http://127.0.0.1:18081/realms/a")
	assert.NoError(t, err, "http accepté en local seulement")

	// Protection SSRF : une adresse interne n'est jamais contactée en production.
	idp := newFakeIdP(t)
	_, err = strict.Discover(context.Background(), idp.srv.URL)
	assert.ErrorIs(t, err, ErrDiscovery)
	assert.Contains(t, err.Error(), "private address", "refus à la connexion, pas une erreur réseau quelconque")
	_, err = NewOIDC(true).Discover(context.Background(), idp.srv.URL)
	assert.NoError(t, err)
}

func TestCandidates(t *testing.T) {
	assert.Equal(t, []string{"equipe.exemple.sn", "exemple.sn"}, candidates("Awa@Equipe.Exemple.SN"))
	assert.Equal(t, []string{"exemple.sn"}, candidates("awa@exemple.sn"))
	assert.Nil(t, candidates("pas-une-adresse"))
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	return f[name], nil
}

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

// Configuration et domaines sur un vrai Postgres (IAM_E2E=1).
func TestE2E_Store(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	org, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{org, other} {
		require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'sso')`, id).Error)
	}
	sealer, err := auth.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.NoError(t, err)
	idp := newFakeIdP(t)
	dns := fakeResolver{}
	s := &Store{DB: db, Sealer: sealer, OIDC: NewOIDC(true), Resolver: dns, RedirectURI: "http://localhost:3000/auth/sso/callback"}
	caller := iam.UserPrincipal(uuid.New())
	domain := "sso-" + uuid.NewString()[:8] + ".sn"
	email := "awa@equipe." + domain

	secret := "s3cret"
	_, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "betorun", Enabled: true})
	assert.ErrorIs(t, err, ErrClientSecret, "secret exigé à la création")
	_, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "betorun", ClientSecret: &secret, JITRole: "roles/owner"})
	assert.ErrorIs(t, err, ErrJITRole)
	_, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL + "/inconnu", ClientID: "betorun", ClientSecret: &secret, Enabled: true})
	assert.ErrorIs(t, err, ErrDiscovery, "activer vérifie le fournisseur")
	cfg, err := s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "betorun", ClientSecret: &secret, Enabled: true, JITRole: "roles/viewer"})
	require.NoError(t, err)
	assert.True(t, cfg.HasSecret && cfg.Enabled && !cfg.Enforced)
	_, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "betorun", Enabled: true, Enforced: true})
	assert.ErrorIs(t, err, ErrEnforceUntested, "pas d'obligation sans domaine prouvé ni connexion réussie")

	// Domaine : revendiqué, prouvé par TXT, unique une fois prouvé.
	d, err := s.AddDomain(ctx, org, domain)
	require.NoError(t, err)
	assert.Equal(t, ChallengePrefix+domain, d.RecordName)
	_, err = s.ForEmail(ctx, email)
	assert.ErrorIs(t, err, ErrNoSSO, "domaine pas encore prouvé")
	_, err = s.VerifyDomain(ctx, org, domain)
	assert.ErrorIs(t, err, ErrDomainUnproven)
	_, err = s.AddDomain(ctx, other, domain)
	require.NoError(t, err, "revendiquer n'est pas prouver")
	dns[d.RecordName] = []string{d.RecordValue}
	v, err := s.VerifyDomain(ctx, org, domain)
	require.NoError(t, err)
	assert.True(t, v.Verified)
	_, err = s.AddDomain(ctx, other, domain)
	assert.ErrorIs(t, err, ErrDomainTaken)

	login, err := s.ForEmail(ctx, email)
	require.NoError(t, err, "sous-domaine de l'adresse accepté")
	assert.Equal(t, org, login.Organization)
	assert.Equal(t, "s3cret", login.Client.ClientSecret, "secret déchiffré")
	required, err := s.Required(ctx, email)
	require.NoError(t, err)
	assert.False(t, required)

	// Rôle de première connexion : une fois, pas sur un membre existant.
	user := uuid.New()
	require.NoError(t, s.GrantFirstAccess(ctx, login, user))
	var roles []string
	require.NoError(t, db.Raw(`SELECT role FROM iam_bindings WHERE resource_name = ? AND principal = ?`,
		string(iam.OrganizationName(org)), string(iam.UserPrincipal(user))).Scan(&roles).Error)
	assert.Equal(t, []string{"roles/viewer"}, roles)
	login.JITRole = "roles/editor"
	require.NoError(t, s.GrantFirstAccess(ctx, login, user))
	require.NoError(t, db.Raw(`SELECT role FROM iam_bindings WHERE resource_name = ? AND principal = ?`,
		string(iam.OrganizationName(org)), string(iam.UserPrincipal(user))).Scan(&roles).Error)
	assert.Equal(t, []string{"roles/viewer"}, roles, "rôle existant gardé")

	// Obligation : après une connexion réussie.
	s.MarkTested(ctx, org)
	cfg, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "betorun", Enabled: true, Enforced: true, JITRole: "roles/viewer"})
	require.NoError(t, err)
	assert.True(t, cfg.Enforced)
	required, err = s.Required(ctx, email)
	require.NoError(t, err)
	assert.True(t, required)
	assert.ErrorIs(t, s.RemoveDomain(ctx, org, domain), ErrEnforceUntested, "dernier domaine gardé tant que le SSO est imposé")

	// Autre fournisseur : la preuve de connexion ne vaut plus.
	cfg, err = s.Save(ctx, caller, org, Update{Issuer: idp.srv.URL, ClientID: "autre-app", Enabled: true})
	require.NoError(t, err)
	assert.Nil(t, cfg.TestedAt)
	assert.False(t, cfg.Enforced)

	require.NoError(t, s.Delete(ctx, org))
	_, err = s.ForEmail(ctx, email)
	assert.ErrorIs(t, err, ErrNoSSO)
	require.NoError(t, s.RemoveDomain(ctx, org, domain))
	require.NoError(t, s.RemoveDomain(ctx, other, domain))
}
