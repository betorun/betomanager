package iamapi

import (
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware"
)

// ── Double authentification (iam.md §9) ──────────────────────────────────

// MFASettings : niveaux d'authentification (acr) qui attestent une double
// authentification. Les facteurs se gèrent dans la console (GET /identity/mfa).
type MFASettings struct {
	ACRValues []string
	MaxAge    time.Duration
}

// SessionResponse : état d'authentification de l'appelant (console).
type SessionResponse struct {
	Principal  iam.Principal `json:"principal"`
	AuthMethod string        `json:"auth_method"`
	// MFAAvailable : la plateforme sait vérifier la double authentification.
	MFAAvailable bool `json:"mfa_available"`
	// MFA : la session courante a été ouverte avec double authentification.
	MFA      bool       `json:"mfa"`
	AuthTime *time.Time `json:"auth_time,omitempty"`
	// StepUpValidUntil : jusqu'à quand les actions sensibles passent sans
	// nouvelle confirmation.
	StepUpValidUntil *time.Time `json:"step_up_valid_until,omitempty"`
	StepUpMaxAge     int        `json:"step_up_max_age_seconds"`
}

func (h *Controller) Session(c *gin.Context) (*SessionResponse, error) {
	p, err := caller(c)
	if err != nil {
		return nil, err
	}
	out := &SessionResponse{Principal: p, AuthMethod: c.GetString(middleware.ContextKeyAuthMethod),
		MFAAvailable: len(h.MFA.ACRValues) > 0, StepUpMaxAge: int(h.MFA.MaxAge.Seconds())}
	if t := c.GetInt64(middleware.ContextKeyAuthTime); t > 0 {
		at := time.Unix(t, 0)
		out.AuthTime = &at
	}
	out.MFA = out.MFAAvailable && slices.Contains(h.MFA.ACRValues, c.GetString(middleware.ContextKeyAuthAcr))
	if out.MFA && out.AuthTime != nil {
		until := out.AuthTime.Add(h.MFA.MaxAge)
		out.StepUpValidUntil = &until
	}
	return out, nil
}

// ── Politique de sécurité d'une organisation ─────────────────────────────

// SetOrgSecurityPolicyRequest : seuls les champs présents changent.
type SetOrgSecurityPolicyRequest struct {
	OrgID                string    `path:"id" binding:"required"`
	RequireMFA           *bool     `json:"require_mfa,omitempty" description:"Exiger la double authentification de toute personne qui agit sur l'organisation et ses projets"`
	AllowedMemberDomains *[]string `json:"allowed_member_domains,omitempty" description:"Seules les personnes de ces domaines peuvent recevoir un accès (liste vide : tout le monde). Les accès déjà donnés restent."`
	DisableKeyCreation   *bool     `json:"disable_key_creation,omitempty" description:"Interdire les nouvelles clés de compte de service (la fédération d'identité reste possible)"`
	DataAccessLogs       *bool     `json:"data_access_logs,omitempty" description:"Inscrire au journal d'audit les lectures de données (logs, variables, identifiants)"`
}

func (h *Controller) GetOrgSecurityPolicy(c *gin.Context, in *OrgPath) (*iam.OrgSecurityPolicy, error) {
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return h.OrgPolicies.Get(c.Request.Context(), id[0])
}

func (h *Controller) SetOrgSecurityPolicy(c *gin.Context, in *SetOrgSecurityPolicyRequest) (*iam.OrgSecurityPolicy, error) {
	p, err := caller(c)
	if err != nil {
		return nil, err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return h.OrgPolicies.Update(c.Request.Context(), p, id[0], iam.OrgPolicyUpdate{RequireMFA: in.RequireMFA,
		AllowedMemberDomains: in.AllowedMemberDomains, DisableKeyCreation: in.DisableKeyCreation, DataAccessLogs: in.DataAccessLogs})
}
