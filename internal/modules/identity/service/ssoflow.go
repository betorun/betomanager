package services

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/auth/social"
	"github.com/thekrauss/kubemanager/internal/auth/sso"
	"github.com/thekrauss/kubemanager/internal/iam"
	identitytypes "github.com/thekrauss/kubemanager/internal/modules/identity/types"
)

// Connexion d'entreprise (docs/architecture/iam.md §9.2) : l'adresse
// désigne l'organisation qui a prouvé son domaine ; la personne se connecte
// chez le fournisseur de l'organisation, puis revient sur la console
// (/auth/sso/callback).

const (
	pageSSOCallback = "/auth/sso/callback"
	// AMRSSO : session ouverte par la connexion d'entreprise.
	AMRSSO = "sso"
)

type ssoState struct {
	Organization uuid.UUID `json:"organization"`
	Verifier     string    `json:"verifier"`
	Nonce        string    `json:"nonce"`
}

func ssoProvider(org uuid.UUID) string { return "sso:" + org.String() }

func (s *service) ssoReady() error {
	if err := s.authReady(); err != nil {
		return err
	}
	if s.sso == nil || s.identities == nil || s.frontendBaseURL == "" {
		return ErrSocialUnavailable
	}
	return nil
}

// SSORedirectURI : adresse de retour à déclarer chez les fournisseurs.
func (s *service) SSORedirectURI() string { return s.consoleLink(pageSSOCallback, "") }

// SSOStart : page du fournisseur de l'organisation qui a prouvé le domaine
// de l'adresse ; state, PKCE et nonce gardés côté serveur (usage unique, 10 min).
func (s *service) SSOStart(ctx context.Context, in identitytypes.SSOStartRequest, cl Client) (*identitytypes.URLResponse, error) {
	if err := s.ssoReady(); err != nil {
		return nil, err
	}
	if err := s.limit(ctx, "login:ip:"+cl.IP, loginPerIP, loginWindow); err != nil {
		return nil, err
	}
	email := normalizeEmail(in.Email)
	login, err := s.sso.ForEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	state, err := social.NewState()
	if err != nil {
		return nil, err
	}
	nonce, err := social.NewState()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := social.NewPKCE()
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(ssoState{Organization: login.Organization, Verifier: verifier, Nonce: nonce})
	if err := s.stateStore.SetValue(ctx, "auth:sso:"+digest(state), string(raw), socialStateTTL); err != nil {
		return nil, ErrAuthUnavailable
	}
	u, err := s.sso.OIDC.AuthCodeURL(ctx, login.Client, state, challenge, nonce, s.SSORedirectURI(), email)
	if err != nil {
		return nil, err
	}
	return &identitytypes.URLResponse{URL: u}, nil
}

// SSOCallback : code du fournisseur contre une session betorun (ou une
// demande de second facteur, si la personne en a un chez betorun).
func (s *service) SSOCallback(ctx context.Context, in identitytypes.SSOCallbackRequest, cl Client) (*identitytypes.AuthResponse, error) {
	if err := s.ssoReady(); err != nil {
		return nil, err
	}
	if err := s.limit(ctx, "login:ip:"+cl.IP, loginPerIP, loginWindow); err != nil {
		return nil, err
	}
	key := "auth:sso:" + digest(in.State)
	raw, err := s.stateStore.GetValue(ctx, key)
	if err != nil || raw == "" {
		return nil, ErrMFAChallengeExpired
	}
	_ = s.stateStore.DeleteValue(ctx, key) // usage unique
	var st ssoState
	if err := json.Unmarshal([]byte(raw), &st); err != nil || st.Organization == uuid.Nil {
		return nil, ErrMFAChallengeExpired
	}
	audit.Annotate(ctx, "organization", iam.OrganizationName(st.Organization).String())
	login, err := s.sso.ForOrganization(ctx, st.Organization)
	if err != nil {
		return nil, err
	}
	id, err := s.sso.OIDC.Exchange(ctx, login.Client, in.Code, st.Verifier, st.Nonce, s.SSORedirectURI())
	if err != nil {
		audit.Annotate(ctx, "sso", "provider_refused")
		return nil, err
	}
	if id.Email == "" {
		return nil, sso.ErrProviderRefused
	}
	if id.EmailVerified != nil && !*id.EmailVerified {
		return nil, social.ErrEmailUnverified
	}
	// L'adresse doit être sur un domaine prouvé par l'organisation : un
	// fournisseur ne parle que pour ses propres domaines.
	if !iam.DomainAllowed(id.Email, login.Domains) {
		audit.Annotate(ctx, "sso", "email_outside_domains")
		return nil, sso.ErrEmailDomain
	}
	userID, subject, err := s.resolveSocialUser(ctx, &social.Profile{Provider: ssoProvider(st.Organization),
		Subject: id.Subject, Email: id.Email, EmailVerified: true, Name: id.Name})
	if err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "user", "user:"+userID.String())
	if err := s.sso.GrantFirstAccess(ctx, login, userID); err != nil {
		return nil, err
	}
	s.sso.MarkTested(ctx, st.Organization)
	return s.completeLogin(ctx, userID, subject, []string{AMRSSO}, auth.KindWeb, cl)
}

// checkSSORequired : une adresse dont l'organisation impose la connexion
// d'entreprise n'entre pas par un autre moyen (mot de passe, Google, passkey).
func (s *service) checkSSORequired(ctx context.Context, userID uuid.UUID, amr []string) error {
	if s.sso == nil || slices.Contains(amr, AMRSSO) {
		return nil
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		return nil // compte introuvable : l'étape suivante refusera
	}
	required, err := s.sso.Required(ctx, user.Email)
	if err != nil {
		return ErrAuthUnavailable
	}
	if required {
		audit.Annotate(ctx, "login", "sso_required")
		return sso.ErrSSORequired
	}
	return nil
}
