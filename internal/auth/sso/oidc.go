// Package sso : connexion d'entreprise par organisation (OpenID Connect).
// L'organisation branche son fournisseur d'identité (Google Workspace,
// Microsoft Entra ID, Okta, Keycloak…) et prouve ses domaines par DNS ; les
// personnes de ces domaines se connectent alors chez elle
// (docs/architecture/iam.md §9.2).
package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MicahParks/keyfunc/v2"
	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidIssuer   = errors.New("the identity provider address must be an https URL without query or fragment")
	ErrDiscovery       = errors.New("the identity provider could not be reached or its OpenID configuration is invalid")
	ErrProviderRefused = errors.New("your company's identity provider did not confirm your identity")
	errPrivateAddress  = errors.New("sso: refusing to contact a private address")
)

// detailedError : erreur connue, avec un détail pour les journaux (le
// message montré à l'utilisateur reste celui de base).
type detailedError struct {
	base   error
	detail string
}

func (e *detailedError) Error() string { return e.base.Error() + " (" + e.detail + ")" }
func (e *detailedError) Unwrap() error { return e.base }

func detail(base error, d string) error { return &detailedError{base: base, detail: d} }

// Discovery : points d'accès annoncés par le fournisseur.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Identity : personne attestée par le fournisseur (id_token vérifié).
type Identity struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil : claim absent (Entra ID ne l'envoie pas)
	Name          string
	AMR           []string
}

// OIDC : client des fournisseurs, avec cache de découverte et de clés.
type OIDC struct {
	client *http.Client
	// insecure : http et adresses internes acceptés (développement local).
	insecure bool

	mu   sync.Mutex
	disc map[string]cachedDiscovery
	jwks map[string]*keyfunc.JWKS
}

type cachedDiscovery struct {
	d  *Discovery
	at time.Time
}

const discoveryTTL = time.Hour

// NewOIDC : client qui refuse les adresses internes (SSRF), sauf en
// développement local (allowInsecure).
func NewOIDC(allowInsecure bool) *OIDC {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowInsecure {
		dialer.Control = refusePrivate
	}
	return &OIDC{
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, MaxIdleConns: 20,
		}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		insecure: allowInsecure,
		disc:     map[string]cachedDiscovery{},
		jwks:     map[string]*keyfunc.JWKS{},
	}
}

var sharedAddressSpace = net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || sharedAddressSpace.Contains(ip) {
		return errPrivateAddress
	}
	return nil
}

// NormalizeIssuer : URL canonique (sans « / » final) ; https exigé hors
// développement local.
func (o *OIDC) NormalizeIssuer(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || len(raw) > 300 {
		return "", ErrInvalidIssuer
	}
	if u.Scheme != "https" && !(o.insecure && u.Scheme == "http") {
		return "", ErrInvalidIssuer
	}
	return raw, nil
}

func (o *OIDC) endpointOK(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || (o.insecure && u.Scheme == "http"))
}

// Discover : configuration OpenID du fournisseur (mise en cache 1 h).
func (o *OIDC) Discover(ctx context.Context, issuer string) (*Discovery, error) {
	o.mu.Lock()
	c, ok := o.disc[issuer]
	o.mu.Unlock()
	if ok && time.Since(c.at) < discoveryTTL {
		return c.d, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, ErrDiscovery
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, detail(ErrDiscovery, err.Error())
	}
	defer resp.Body.Close()
	var d Discovery
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&d) != nil {
		return nil, detail(ErrDiscovery, fmt.Sprint("status ", resp.StatusCode))
	}
	if strings.TrimRight(d.Issuer, "/") != issuer || !o.endpointOK(d.AuthorizationEndpoint) ||
		!o.endpointOK(d.TokenEndpoint) || !o.endpointOK(d.JWKSURI) {
		return nil, detail(ErrDiscovery, "inconsistent configuration")
	}
	o.mu.Lock()
	o.disc[issuer] = cachedDiscovery{d: &d, at: time.Now()}
	o.mu.Unlock()
	return &d, nil
}

func (o *OIDC) keys(jwksURI string) (*keyfunc.JWKS, error) {
	o.mu.Lock()
	k, ok := o.jwks[jwksURI]
	o.mu.Unlock()
	if ok {
		return k, nil
	}
	k, err := keyfunc.Get(jwksURI, keyfunc.Options{Client: o.client, RefreshInterval: time.Hour,
		RefreshRateLimit: time.Minute, RefreshTimeout: 10 * time.Second, RefreshUnknownKID: true})
	if err != nil {
		return nil, detail(ErrDiscovery, "jwks: "+err.Error())
	}
	o.mu.Lock()
	o.jwks[jwksURI] = k
	o.mu.Unlock()
	return k, nil
}

// Client : application enregistrée chez le fournisseur de l'organisation.
type Client struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// AuthCodeURL : page du fournisseur (code + PKCE S256 + nonce).
func (o *OIDC) AuthCodeURL(ctx context.Context, c Client, state, challenge, nonce, redirectURI, loginHint string) (string, error) {
	d, err := o.Discover(ctx, c.Issuer)
	if err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {redirectURI},
		"scope": {"openid email profile"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Exchange : code contre l'identité attestée (id_token : signature,
// émetteur exact, audience, nonce, dates).
func (o *OIDC) Exchange(ctx context.Context, c Client, code, verifier, nonce, redirectURI string) (*Identity, error) {
	d, err := o.Discover(ctx, c.Issuer)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, detail(ErrProviderRefused, err.Error())
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok) != nil || resp.StatusCode >= 400 || tok.IDToken == "" {
		return nil, detail(ErrProviderRefused, fmt.Sprint("token endpoint: ", resp.StatusCode, " ", tok.Error))
	}
	keys, err := o.keys(d.JWKSURI)
	if err != nil {
		return nil, err
	}
	var claims struct {
		jwt.RegisteredClaims
		Email         string   `json:"email"`
		EmailVerified *bool    `json:"email_verified"`
		Name          string   `json:"name"`
		Nonce         string   `json:"nonce"`
		AMR           []string `json:"amr"`
		// Entra ID : l'adresse peut n'être que dans preferred_username.
		PreferredUsername string `json:"preferred_username"`
	}
	_, err = jwt.ParseWithClaims(tok.IDToken, &claims, keys.Keyfunc,
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "PS256"}),
		jwt.WithIssuer(d.Issuer), jwt.WithAudience(c.ClientID), jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return nil, detail(ErrProviderRefused, err.Error())
	}
	if claims.Nonce == "" || claims.Nonce != nonce || claims.Subject == "" {
		return nil, ErrProviderRefused
	}
	email := claims.Email
	if email == "" && strings.Contains(claims.PreferredUsername, "@") {
		email = claims.PreferredUsername
	}
	return &Identity{Subject: claims.Subject, Email: strings.ToLower(strings.TrimSpace(email)),
		EmailVerified: claims.EmailVerified, Name: claims.Name, AMR: claims.AMR}, nil
}
