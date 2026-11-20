package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/auth"
	identitytypes "github.com/thekrauss/kubemanager/internal/modules/identity/types"
)

// Passkeys (docs/architecture/auth.md §6) : l'état de chaque cérémonie
// WebAuthn reste côté serveur (Redis, 5 min, usage unique). Le navigateur ne
// reçoit que les options et un identifiant de cérémonie.

const (
	ceremonyTTL       = 5 * time.Minute
	ceremonyRegister  = "register"
	ceremonyLogin     = "login"
	ceremonyMFA       = "mfa"
	ceremonyStepUp    = "stepup"
	passkeyLoginPerIP = 30
)

// ErrCeremonyExpired : cérémonie inconnue, déjà utilisée ou expirée.
var ErrCeremonyExpired = errors.New("the passkey request has expired, try again")

type ceremony struct {
	Purpose string    `json:"purpose"`
	UserID  uuid.UUID `json:"user_id,omitempty"`
	// SessionID : session qui a lancé l'enregistrement ou la vérification.
	SessionID uuid.UUID `json:"session_id,omitempty"`
	// Challenge : connexion en attente du second facteur (mfaChallenge).
	Challenge string `json:"challenge,omitempty"`
	State     []byte `json:"state"`
}

func (s *service) passkeysReady() error {
	if err := s.authReady(); err != nil {
		return err
	}
	if s.factors.Passkeys == nil || s.factors.Passkeys.WebAuthn == nil {
		return auth.ErrPasskeyUnavailable
	}
	return nil
}

func (s *service) saveCeremony(ctx context.Context, c ceremony, options json.RawMessage) (*identitytypes.PasskeyOptions, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if err := s.stateStore.SetValue(ctx, "auth:webauthn:"+id, string(raw), ceremonyTTL); err != nil {
		return nil, ErrAuthUnavailable
	}
	return &identitytypes.PasskeyOptions{Ceremony: id, Options: options}, nil
}

// takeCeremony : lue puis effacée (usage unique), quel que soit le résultat.
func (s *service) takeCeremony(ctx context.Context, id, purpose string) (*ceremony, error) {
	key := "auth:webauthn:" + strings.TrimSpace(id)
	raw, err := s.stateStore.GetValue(ctx, key)
	if err != nil || raw == "" {
		return nil, ErrCeremonyExpired
	}
	_ = s.stateStore.DeleteValue(ctx, key)
	var c ceremony
	if err := json.Unmarshal([]byte(raw), &c); err != nil || c.Purpose != purpose {
		return nil, ErrCeremonyExpired
	}
	return &c, nil
}

// ── Gestion (page Sécurité) ──

func (s *service) ListPasskeys(ctx context.Context, userID uuid.UUID) ([]auth.Passkey, error) {
	if err := s.authReady(); err != nil {
		return nil, err
	}
	return s.factors.Passkeys.List(ctx, userID)
}

// BeginPasskeyRegistration : un compte qui a déjà un second facteur doit
// l'avoir vérifié récemment (comme pour gérer ses autres facteurs).
func (s *service) BeginPasskeyRegistration(ctx context.Context, userID, sessionID uuid.UUID) (*identitytypes.PasskeyOptions, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	if has, err := s.factors.HasFactor(ctx, userID); err != nil {
		return nil, err
	} else if has {
		if err := s.requireRecentMFA(ctx, userID, sessionID); err != nil {
			return nil, err
		}
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		return nil, ErrAuthUnavailable
	}
	opts, state, err := s.factors.Passkeys.BeginRegistration(ctx, userID, user.Email, firstNonEmpty(user.FullName, user.Email))
	if err != nil {
		return nil, err
	}
	return s.saveCeremony(ctx, ceremony{Purpose: ceremonyRegister, UserID: userID, SessionID: sessionID, State: state}, opts)
}

// FinishPasskeyRegistration : la passkey compte comme second facteur ; la
// première, sans application d'authentification, remet des codes de secours.
func (s *service) FinishPasskeyRegistration(ctx context.Context, userID, sessionID uuid.UUID, in identitytypes.PasskeyRegisterRequest) (*identitytypes.PasskeyRegistered, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	c, err := s.takeCeremony(ctx, in.Ceremony, ceremonyRegister)
	if err != nil {
		return nil, err
	}
	if c.UserID != userID || c.SessionID != sessionID {
		return nil, ErrCeremonyExpired
	}
	pk, err := s.factors.Passkeys.FinishRegistration(ctx, userID, c.State, in.Credential, in.Name)
	if err != nil {
		return nil, err
	}
	out := &identitytypes.PasskeyRegistered{Passkey: identitytypes.Passkey(*pk)}
	if out.RecoveryCodes, err = s.factors.EnsureRecovery(ctx, userID); err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "mfa", "passkey_added")
	s.securityMail(ctx, userID, emailPasskeyAdded, s.consoleLink(pageSecurity, ""), map[string]string{"Passkey": pk.Name})
	return out, nil
}

func (s *service) RenamePasskey(ctx context.Context, userID, id uuid.UUID, name string) error {
	if err := s.authReady(); err != nil {
		return err
	}
	return s.factors.Passkeys.Rename(ctx, userID, id, name)
}

// RemovePasskey : vérification récente exigée.
func (s *service) RemovePasskey(ctx context.Context, userID, sessionID, id uuid.UUID) error {
	if err := s.requireRecentMFA(ctx, userID, sessionID); err != nil {
		return err
	}
	if err := s.factors.RemovePasskey(ctx, userID, id); err != nil {
		return err
	}
	audit.Annotate(ctx, "mfa", "passkey_removed")
	s.securityMail(ctx, userID, emailPasskeyRemoved, s.consoleLink(pageSecurity, ""), nil)
	return nil
}

// ── Connexion sans mot de passe ──

func (s *service) BeginPasskeyLogin(ctx context.Context, cl Client) (*identitytypes.PasskeyOptions, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	if err := s.limit(ctx, "passkey:ip:"+cl.IP, passkeyLoginPerIP, loginWindow); err != nil {
		return nil, err
	}
	opts, state, err := s.factors.Passkeys.BeginLogin(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	return s.saveCeremony(ctx, ceremony{Purpose: ceremonyLogin, State: state}, opts)
}

// FinishPasskeyLogin : la passkey vérifie l'utilisateur (biométrie ou code
// de l'appareil) : la session est ouverte au niveau « second facteur ».
func (s *service) FinishPasskeyLogin(ctx context.Context, in identitytypes.PasskeyLoginRequest, cl Client) (*identitytypes.AuthResponse, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	c, err := s.takeCeremony(ctx, in.Ceremony, ceremonyLogin)
	if err != nil {
		return nil, err
	}
	userID, err := s.factors.Passkeys.FinishLogin(ctx, uuid.Nil, c.State, in.Credential)
	if err != nil {
		audit.Annotate(ctx, "login", "failed")
		return nil, err
	}
	subject, err := s.activeAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "user", "user:"+userID.String())
	if err := s.checkSSORequired(ctx, userID, []string{auth.AMRPasskey}); err != nil {
		return nil, err
	}
	kind := sessionKind(in.Client)
	t, err := s.sessions.Create(ctx, auth.NewSession{UserID: userID, Kind: kind, AMR: []string{auth.AMRPasskey}, MFA: true,
		IP: cl.IP, UserAgent: cl.UserAgent})
	if err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "login", "succeeded")
	audit.Annotate(ctx, "mfa_method", auth.MethodPasskey)
	s.noticeLogin(ctx, userID, kind, auth.MethodPasskey, cl)
	if user, err := s.users.FindByKeycloakUserID(ctx, subject); err == nil && user != nil {
		s.touchLastLogin(ctx, user)
	}
	return authResponse(t), nil
}

// activeAccount : compte actif ici et dans l'annuaire (un compte désactivé
// ne se connecte pas plus par passkey que par mot de passe).
func (s *service) activeAccount(ctx context.Context, userID uuid.UUID) (string, error) {
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user == nil || !user.IsActive {
		return "", auth.ErrPasskeyInvalid
	}
	kc, err := s.keycloak.GetUserByID(ctx, user.KeycloakUserID)
	if err != nil {
		return "", ErrAuthUnavailable
	}
	if !kc.Enabled {
		return "", auth.ErrPasskeyInvalid
	}
	return user.KeycloakUserID, nil
}

// ── Second facteur après le mot de passe ──

func (s *service) mfaChallengeOf(ctx context.Context, challenge string) (*mfaChallenge, error) {
	raw, err := s.stateStore.GetValue(ctx, "auth:mfa:"+strings.TrimSpace(challenge))
	if err != nil || raw == "" {
		return nil, ErrMFAChallengeExpired
	}
	var ch mfaChallenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		return nil, ErrMFAChallengeExpired
	}
	return &ch, nil
}

func (s *service) BeginPasskeyMFA(ctx context.Context, challenge string) (*identitytypes.PasskeyOptions, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	ch, err := s.mfaChallengeOf(ctx, challenge)
	if err != nil {
		return nil, err
	}
	opts, state, err := s.factors.Passkeys.BeginLogin(ctx, ch.UserID)
	if err != nil {
		return nil, err
	}
	return s.saveCeremony(ctx, ceremony{Purpose: ceremonyMFA, UserID: ch.UserID, Challenge: challenge, State: state}, opts)
}

func (s *service) FinishPasskeyMFA(ctx context.Context, in identitytypes.PasskeyMFARequest, cl Client) (*identitytypes.AuthResponse, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	c, err := s.takeCeremony(ctx, in.Ceremony, ceremonyMFA)
	if err != nil {
		return nil, err
	}
	if c.Challenge != strings.TrimSpace(in.Challenge) {
		return nil, ErrCeremonyExpired
	}
	ch, err := s.mfaChallengeOf(ctx, in.Challenge)
	if err != nil || ch.UserID != c.UserID {
		return nil, ErrMFAChallengeExpired
	}
	if err := s.limit(ctx, "mfa:"+in.Challenge, mfaAttempts, mfaChallengeTTL); err != nil {
		_ = s.stateStore.DeleteValue(ctx, "auth:mfa:"+in.Challenge)
		return nil, ErrMFAChallengeExpired
	}
	audit.Annotate(ctx, "user", "user:"+ch.UserID.String())
	if _, err := s.factors.Passkeys.FinishLogin(ctx, ch.UserID, c.State, in.Credential); err != nil {
		audit.Annotate(ctx, "mfa", "failed")
		return nil, err
	}
	_ = s.stateStore.DeleteValue(ctx, "auth:mfa:"+in.Challenge)
	amr := ch.AMR
	if len(amr) == 0 {
		amr = []string{auth.AMRPassword}
	}
	tokens, err := s.sessions.Create(ctx, auth.NewSession{UserID: ch.UserID, Kind: ch.Kind,
		AMR: append(amr, auth.AMRPasskey), MFA: true, IP: cl.IP, UserAgent: cl.UserAgent})
	if err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "login", "succeeded")
	audit.Annotate(ctx, "mfa_method", auth.MethodPasskey)
	s.noticeLogin(ctx, ch.UserID, ch.Kind, auth.MethodPasskey, cl)
	if user, err := s.users.FindByKeycloakUserID(ctx, ch.Subject); err == nil && user != nil {
		s.touchLastLogin(ctx, user)
	}
	return authResponse(tokens), nil
}

// ── Vérification avant une action sensible ──

func (s *service) BeginPasskeyStepUp(ctx context.Context, userID, sessionID uuid.UUID) (*identitytypes.PasskeyOptions, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	if err := s.limit(ctx, "stepup:"+sessionID.String(), stepUpPerSession, stepUpWindow); err != nil {
		return nil, err
	}
	opts, state, err := s.factors.Passkeys.BeginLogin(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.saveCeremony(ctx, ceremony{Purpose: ceremonyStepUp, UserID: userID, SessionID: sessionID, State: state}, opts)
}

func (s *service) FinishPasskeyStepUp(ctx context.Context, userID, sessionID uuid.UUID, in identitytypes.PasskeyStepUpRequest) (*identitytypes.AuthResponse, error) {
	if err := s.passkeysReady(); err != nil {
		return nil, err
	}
	c, err := s.takeCeremony(ctx, in.Ceremony, ceremonyStepUp)
	if err != nil {
		return nil, err
	}
	if c.UserID != userID || c.SessionID != sessionID {
		return nil, ErrCeremonyExpired
	}
	if _, err := s.factors.Passkeys.FinishLogin(ctx, userID, c.State, in.Credential); err != nil {
		audit.Annotate(ctx, "mfa", "failed")
		return nil, err
	}
	audit.Annotate(ctx, "mfa_method", auth.MethodPasskey)
	t, err := s.sessions.MarkMFA(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	return authResponse(t), nil
}
