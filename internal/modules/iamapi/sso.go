package iamapi

import (
	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/auth/sso"
)

// ── Connexion d'entreprise d'une organisation (iam.md §9.2) ──────────────

type SetOrgSSORequest struct {
	OrgID        string  `path:"id" binding:"required"`
	Issuer       string  `json:"issuer" binding:"required" description:"Adresse de l'émetteur OpenID Connect (ex. https://login.microsoftonline.com/<tenant>/v2.0)"`
	ClientID     string  `json:"client_id" binding:"required" description:"Identifiant de l'application déclarée chez le fournisseur"`
	ClientSecret *string `json:"client_secret,omitempty" description:"Secret de l'application (absent : inchangé). Jamais relu."`
	Enabled      bool    `json:"enabled" description:"Proposer la connexion d'entreprise aux adresses des domaines vérifiés"`
	Enforced     bool    `json:"enforced" description:"Imposer la connexion d'entreprise à ces adresses (mot de passe, Google, GitHub et passkey refusés)"`
	JITRole      string  `json:"jit_role" description:"Rôle donné sur l'organisation à la première connexion : roles/viewer, roles/editor ou vide"`
}

type OrgDomainRequest struct {
	OrgID  string `path:"id" binding:"required"`
	Domain string `json:"domain" binding:"required" description:"Domaine des adresses (ex. exemple.sn)"`
}

type OrgDomainPath struct {
	OrgID  string `path:"id" binding:"required"`
	Domain string `path:"domain" binding:"required"`
}

func (h *Controller) ssoStore() (*sso.Store, error) {
	if h.SSO == nil {
		return nil, sso.ErrNotConfigured
	}
	return h.SSO, nil
}

func (h *Controller) GetOrgSSO(c *gin.Context, in *OrgPath) (*sso.Config, error) {
	st, err := h.ssoStore()
	if err != nil {
		return nil, err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return st.Get(c.Request.Context(), id[0])
}

func (h *Controller) SetOrgSSO(c *gin.Context, in *SetOrgSSORequest) (*sso.Config, error) {
	st, err := h.ssoStore()
	if err != nil {
		return nil, err
	}
	p, err := caller(c)
	if err != nil {
		return nil, err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return st.Save(c.Request.Context(), p, id[0], sso.Update{Issuer: in.Issuer, ClientID: in.ClientID,
		ClientSecret: in.ClientSecret, Enabled: in.Enabled, Enforced: in.Enforced, JITRole: in.JITRole})
}

func (h *Controller) DeleteOrgSSO(c *gin.Context, in *OrgPath) error {
	st, err := h.ssoStore()
	if err != nil {
		return err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return err
	}
	return st.Delete(c.Request.Context(), id[0])
}

func (h *Controller) AddOrgDomain(c *gin.Context, in *OrgDomainRequest) (*sso.Domain, error) {
	st, err := h.ssoStore()
	if err != nil {
		return nil, err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return st.AddDomain(c.Request.Context(), id[0], in.Domain)
}

func (h *Controller) VerifyOrgDomain(c *gin.Context, in *OrgDomainPath) (*sso.Domain, error) {
	st, err := h.ssoStore()
	if err != nil {
		return nil, err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return nil, err
	}
	return st.VerifyDomain(c.Request.Context(), id[0], in.Domain)
}

func (h *Controller) RemoveOrgDomain(c *gin.Context, in *OrgDomainPath) error {
	st, err := h.ssoStore()
	if err != nil {
		return err
	}
	id, err := ids(in.OrgID)
	if err != nil {
		return err
	}
	return st.RemoveDomain(c.Request.Context(), id[0], in.Domain)
}
