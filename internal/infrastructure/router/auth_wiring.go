package router

import (
	"fmt"
	"net"
	"strings"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/auth/social"
	"github.com/thekrauss/kubemanager/internal/auth/sso"
	"github.com/thekrauss/kubemanager/internal/core/configs"
	identitySvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
)

// authDeps : authentification gérée par betorun (docs/architecture/auth.md).
// Sessions et second facteur exigent le STS (jetons) et sa clé de
// chiffrement ; l'envoi d'emails n'est actif que s'il est configuré.
func (a *App) authDeps(deps *identitySvc.Deps) error {
	if a.STS == nil {
		return fmt.Errorf("auth: STS required for sessions")
	}
	sealer, err := auth.NewSealer(a.Config.IAM.STSKeyEncryptionKey)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	deps.Sessions = &auth.Sessions{DB: a.DB, Minter: a.STS}
	deps.Factors = &auth.Factors{DB: a.DB, Sealer: sealer, Pepper: []byte(a.Config.IAM.APIKeyPepper)}
	deps.Factors.Passkeys = &auth.Passkeys{DB: a.DB}
	if wa, err := auth.NewWebAuthn(deps.FrontendBaseURL, a.Config.Frontend.MobilePasskeyOrigins...); err == nil {
		deps.Factors.Passkeys.WebAuthn = wa
	} else {
		a.Logger.Warnw("auth: passkeys disabled (console address missing or invalid)", "error", err)
	}
	deps.EmailTokens = &auth.EmailTokens{DB: a.DB}
	deps.Limiter = a.Cache
	deps.Logger = a.Logger
	deps.Identities = &auth.Identities{DB: a.DB}
	deps.Devices = &auth.Devices{DB: a.DB}
	deps.Spawner = a.spawner()
	deps.SocialProviders = socialProviders(a.Config.Social)
	if a.Config.SSO.AllowInsecureIssuers {
		a.Logger.Warnw("sso: insecure identity providers allowed (http, private addresses) — local development only")
	}
	a.SSO = &sso.Store{DB: a.DB, Sealer: sealer, OIDC: sso.NewOIDC(a.Config.SSO.AllowInsecureIssuers),
		Resolver: net.DefaultResolver, RedirectURI: strings.TrimRight(deps.FrontendBaseURL, "/") + "/auth/sso/callback"}
	deps.SSO = a.SSO

	sender, err := a.mailSender()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if sender == nil {
		a.Logger.Warnw("auth: mail disabled — registration, email verification and password reset are unavailable")
		return nil
	}
	deps.Mailer = sender
	return nil
}

// socialProviders : fournisseurs dont l'application OAuth est configurée.
func socialProviders(cfg configs.SocialConfig) map[string]social.Provider {
	out := map[string]social.Provider{}
	conv := func(p configs.SocialProviderConfig) social.Config {
		return social.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, AuthURL: p.AuthURL, TokenURL: p.TokenURL,
			APIURL: p.APIURL, JWKSURL: p.JWKSURL, Issuer: p.Issuer}
	}
	if cfg.Google.ClientID != "" && cfg.Google.ClientSecret != "" {
		out[social.Google] = social.NewGoogle(conv(cfg.Google))
	}
	if cfg.GitHub.ClientID != "" && cfg.GitHub.ClientSecret != "" {
		out[social.GitHub] = social.NewGitHub(conv(cfg.GitHub))
	}
	return out
}
