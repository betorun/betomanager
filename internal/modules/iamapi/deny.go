package iamapi

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// ── Politiques de refus (iam.md §14) ─────────────────────────────────────
// Sur une organisation (/organizations/:id/denyPolicies) ou un projet
// (/projects/:id/denyPolicies). L'écriture exige la double authentification
// récente (iam.denyPolicies.update est sensible).

type DenyPoliciesResponse struct {
	// Policies : politiques posées sur la ressource.
	Policies []iam.DenyPolicy `json:"policies"`
	// Inherited : politiques des parents qui s'appliquent aussi (projet).
	Inherited []iam.DenyPolicy `json:"inherited"`
	// Members : nom et e-mail des principals cités (affichage).
	Members []iam.MemberInfo `json:"members"`
}

type DenyPolicyPath struct {
	ID       string `path:"id" binding:"required"`
	PolicyID string `path:"policyID" binding:"required"`
}

type CreateDenyPolicyRequest struct {
	ID string `path:"id" binding:"required"`
	iam.DenyPolicyInput
}

type UpdateDenyPolicyRequest struct {
	ID       string `path:"id" binding:"required"`
	PolicyID string `path:"policyID" binding:"required"`
	Etag     string `json:"etag" binding:"required" description:"Etag lu par GET (modification concurrente refusée)"`
	iam.DenyPolicyInput
}

// denyResource : organisation ou projet selon la route.
func denyResource(c *gin.Context, id string) (iam.ResourceName, error) {
	u, err := ids(id)
	if err != nil {
		return "", err
	}
	if strings.Contains(c.FullPath(), "/projects/:id/denyPolicies") {
		return iam.ProjectName(u[0]), nil
	}
	return iam.OrganizationName(u[0]), nil
}

func (h *Controller) ListDenyPolicies(c *gin.Context, in *ProjectPath) (*DenyPoliciesResponse, error) {
	r, err := denyResource(c, in.ProjectID)
	if err != nil {
		return nil, err
	}
	own, err := h.DenyPolicies.List(c.Request.Context(), r)
	if err != nil {
		return nil, err
	}
	all, err := h.DenyPolicies.Effective(c.Request.Context(), r)
	if err != nil {
		return nil, err
	}
	out := &DenyPoliciesResponse{Policies: own, Inherited: []iam.DenyPolicy{}}
	var principals []iam.Principal
	for _, d := range all {
		if d.Resource != r {
			out.Inherited = append(out.Inherited, d)
		}
		principals = append(append(principals, d.DeniedPrincipals...), d.ExceptionPrincipals...)
	}
	if out.Members, err = iam.DescribeMembers(c.Request.Context(), h.DB, principals); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Controller) GetDenyPolicy(c *gin.Context, in *DenyPolicyPath) (*iam.DenyPolicy, error) {
	r, err := denyResource(c, in.ID)
	if err != nil {
		return nil, err
	}
	id, err := ids(in.PolicyID)
	if err != nil {
		return nil, err
	}
	return h.DenyPolicies.Get(c.Request.Context(), r, id[0])
}

func (h *Controller) CreateDenyPolicy(c *gin.Context, in *CreateDenyPolicyRequest) (*iam.DenyPolicy, error) {
	p, err := caller(c)
	if err != nil {
		return nil, err
	}
	r, err := denyResource(c, in.ID)
	if err != nil {
		return nil, err
	}
	return h.DenyPolicies.Create(c.Request.Context(), p, r, in.DenyPolicyInput)
}

func (h *Controller) UpdateDenyPolicy(c *gin.Context, in *UpdateDenyPolicyRequest) (*iam.DenyPolicy, error) {
	p, err := caller(c)
	if err != nil {
		return nil, err
	}
	r, err := denyResource(c, in.ID)
	if err != nil {
		return nil, err
	}
	id, err := ids(in.PolicyID)
	if err != nil {
		return nil, err
	}
	return h.DenyPolicies.Update(c.Request.Context(), p, r, id[0], in.Etag, in.DenyPolicyInput)
}

func (h *Controller) DeleteDenyPolicy(c *gin.Context, in *DenyPolicyPath) error {
	p, err := caller(c)
	if err != nil {
		return err
	}
	r, err := denyResource(c, in.ID)
	if err != nil {
		return err
	}
	id, err := ids(in.PolicyID)
	if err != nil {
		return err
	}
	return h.DenyPolicies.Delete(c.Request.Context(), p, r, id[0])
}
