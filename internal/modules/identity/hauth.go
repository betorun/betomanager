package identity

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	identitysvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
	identitytypes "github.com/thekrauss/kubemanager/internal/modules/identity/types"
)

// Authentification gérée par betorun (docs/architecture/auth.md) : routes
// appelées par la console (Next.js) et la CLI.

// AuthController : connexion, sessions, second facteur, emails.
type AuthController struct {
	service identitysvc.Service
}

func NewAuthController(service identitysvc.Service) *AuthController {
	return &AuthController{service: service}
}

func clientOf(c *gin.Context) identitysvc.Client {
	return identitysvc.Client{IP: c.ClientIP(), UserAgent: c.Request.UserAgent()}
}

// session : personne et session de la requête (jeton de session betorun).
func session(c *gin.Context) (uuid.UUID, uuid.UUID, error) {
	user, ok := security.InternalUserID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeUnauthorized, "authenticated user context missing")
	}
	sid, ok := security.SessionID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeUnauthorized, "sign in again to manage your security settings")
	}
	return user, sid, nil
}

func (a *AuthController) Login(c *gin.Context, in *identitytypes.PasswordLoginRequest) (*identitytypes.AuthResponse, error) {
	return a.service.PasswordLogin(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) LoginMFA(c *gin.Context, in *identitytypes.MFALoginRequest) (*identitytypes.AuthResponse, error) {
	return a.service.MFALogin(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) StepUp(c *gin.Context, in *identitytypes.StepUpRequest) (*identitytypes.AuthResponse, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.StepUp(c.Request.Context(), user, sid, in.Code)
}

func (a *AuthController) ListSessions(c *gin.Context) ([]auth.Session, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.ListSessions(c.Request.Context(), user, sid)
}

func (a *AuthController) RevokeSession(c *gin.Context, in *identitytypes.SessionPath) error {
	user, _, err := session(c)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return auth.ErrSessionNotFound
	}
	return a.service.RevokeSession(c.Request.Context(), user, id)
}

func (a *AuthController) RevokeOtherSessions(c *gin.Context) error {
	user, sid, err := session(c)
	if err != nil {
		return err
	}
	return a.service.RevokeOtherSessions(c.Request.Context(), user, sid)
}

func (a *AuthController) MFAStatus(c *gin.Context) (*auth.FactorStatus, error) {
	user, _, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.MFAStatus(c.Request.Context(), user)
}

func (a *AuthController) StartTOTP(c *gin.Context) (*auth.TOTPEnrollment, error) {
	user, _, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.StartTOTP(c.Request.Context(), user, c.GetString("email"))
}

func (a *AuthController) ConfirmTOTP(c *gin.Context, in *identitytypes.TOTPConfirmRequest) (*identitytypes.RecoveryCodesResponse, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.ConfirmTOTP(c.Request.Context(), user, sid, in.Code)
}

func (a *AuthController) DisableTOTP(c *gin.Context) error {
	user, sid, err := session(c)
	if err != nil {
		return err
	}
	return a.service.DisableTOTP(c.Request.Context(), user, sid)
}

func (a *AuthController) RegenerateRecoveryCodes(c *gin.Context) (*identitytypes.RecoveryCodesResponse, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.RegenerateRecoveryCodes(c.Request.Context(), user, sid)
}

func (a *AuthController) VerifyEmail(c *gin.Context, in *identitytypes.VerifyEmailRequest) error {
	return a.service.VerifyEmail(c.Request.Context(), in.Token)
}

func (a *AuthController) ResetPassword(c *gin.Context, in *identitytypes.ResetPasswordRequest) error {
	return a.service.CompletePasswordReset(c.Request.Context(), in.Token, in.Password)
}

// ── Connexion de la CLI (docs/architecture/auth.md §9) ──

func (a *AuthController) CLIAuthorizeURL(c *gin.Context, in *identitytypes.CLIAuthorizeQuery) (*identitytypes.URLResponse, error) {
	return a.service.CLIAuthorizeURL(c.Request.Context(), *in)
}

func (a *AuthController) AuthorizeCLI(c *gin.Context, in *identitytypes.CLIAuthorizeRequest) (*identitytypes.URLResponse, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.AuthorizeCLI(c.Request.Context(), user, sid, *in)
}

func (a *AuthController) CLIToken(c *gin.Context, in *identitytypes.CLITokenRequest) (*identitytypes.AuthResponse, error) {
	return a.service.ExchangeCLICode(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) DeviceInfo(c *gin.Context, in *identitytypes.DeviceCodeQuery) (*identitytypes.DeviceInfo, error) {
	if _, _, err := session(c); err != nil {
		return nil, err
	}
	return a.service.DeviceInfo(c.Request.Context(), in.UserCode)
}

func (a *AuthController) ApproveDevice(c *gin.Context, in *identitytypes.DeviceApproveRequest) error {
	user, sid, err := session(c)
	if err != nil {
		return err
	}
	return a.service.ApproveDevice(c.Request.Context(), user, sid, *in)
}

// ── Connexion Google / GitHub (docs/architecture/auth.md §8) ──

func (a *AuthController) SocialProviders(_ *gin.Context) (*identitytypes.SocialProvidersResponse, error) {
	return &identitytypes.SocialProvidersResponse{Providers: a.service.SocialProviders()}, nil
}

func (a *AuthController) SocialAuthorize(c *gin.Context, in *identitytypes.SocialProviderPath) (*identitytypes.URLResponse, error) {
	return a.service.SocialAuthorizeURL(c.Request.Context(), in.Provider)
}

func (a *AuthController) SocialCallback(c *gin.Context, in *identitytypes.SocialCallbackRequest) (*identitytypes.AuthResponse, error) {
	return a.service.SocialCallback(c.Request.Context(), in.Provider, *in, clientOf(c))
}

func (a *AuthController) SSOStart(c *gin.Context, in *identitytypes.SSOStartRequest) (*identitytypes.URLResponse, error) {
	return a.service.SSOStart(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) SSOCallback(c *gin.Context, in *identitytypes.SSOCallbackRequest) (*identitytypes.AuthResponse, error) {
	return a.service.SSOCallback(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) ListIdentities(c *gin.Context) ([]auth.LinkedIdentity, error) {
	user, _, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.ListIdentities(c.Request.Context(), user)
}

func (a *AuthController) UnlinkIdentity(c *gin.Context, in *identitytypes.SocialProviderPath) error {
	user, sid, err := session(c)
	if err != nil {
		return err
	}
	return a.service.UnlinkIdentity(c.Request.Context(), user, sid, c.GetString("userID"), in.Provider)
}

// ── Passkeys (WebAuthn) ──

func (a *AuthController) ListPasskeys(c *gin.Context) ([]auth.Passkey, error) {
	user, _, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.ListPasskeys(c.Request.Context(), user)
}

func (a *AuthController) PasskeyRegistrationOptions(c *gin.Context) (*identitytypes.PasskeyOptions, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.BeginPasskeyRegistration(c.Request.Context(), user, sid)
}

func (a *AuthController) RegisterPasskey(c *gin.Context, in *identitytypes.PasskeyRegisterRequest) (*identitytypes.PasskeyRegistered, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.FinishPasskeyRegistration(c.Request.Context(), user, sid, *in)
}

func passkeyID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, auth.ErrPasskeyNotFound
	}
	return id, nil
}

func (a *AuthController) RenamePasskey(c *gin.Context, in *identitytypes.PasskeyRenameRequest) error {
	user, _, err := session(c)
	if err != nil {
		return err
	}
	id, err := passkeyID(in.ID)
	if err != nil {
		return err
	}
	return a.service.RenamePasskey(c.Request.Context(), user, id, in.Name)
}

func (a *AuthController) RemovePasskey(c *gin.Context, in *identitytypes.PasskeyPath) error {
	user, sid, err := session(c)
	if err != nil {
		return err
	}
	id, err := passkeyID(in.ID)
	if err != nil {
		return err
	}
	return a.service.RemovePasskey(c.Request.Context(), user, sid, id)
}

func (a *AuthController) PasskeyLoginOptions(c *gin.Context, _ *identitytypes.PasskeyLoginOptionsRequest) (*identitytypes.PasskeyOptions, error) {
	return a.service.BeginPasskeyLogin(c.Request.Context(), clientOf(c))
}

func (a *AuthController) PasskeyLogin(c *gin.Context, in *identitytypes.PasskeyLoginRequest) (*identitytypes.AuthResponse, error) {
	return a.service.FinishPasskeyLogin(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) PasskeyMFAOptions(c *gin.Context, in *identitytypes.PasskeyMFAOptionsRequest) (*identitytypes.PasskeyOptions, error) {
	return a.service.BeginPasskeyMFA(c.Request.Context(), in.Challenge)
}

func (a *AuthController) PasskeyMFA(c *gin.Context, in *identitytypes.PasskeyMFARequest) (*identitytypes.AuthResponse, error) {
	return a.service.FinishPasskeyMFA(c.Request.Context(), *in, clientOf(c))
}

func (a *AuthController) PasskeyStepUpOptions(c *gin.Context) (*identitytypes.PasskeyOptions, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.BeginPasskeyStepUp(c.Request.Context(), user, sid)
}

func (a *AuthController) PasskeyStepUp(c *gin.Context, in *identitytypes.PasskeyStepUpRequest) (*identitytypes.AuthResponse, error) {
	user, sid, err := session(c)
	if err != nil {
		return nil, err
	}
	return a.service.FinishPasskeyStepUp(c.Request.Context(), user, sid, *in)
}
