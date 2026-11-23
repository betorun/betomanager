// Package insights : « L'assistant a remarqué » — constats d'un projet
// (GET /projects/:id/insights), pour la console et le Copilot.
package insights

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/insights/domain"
	"github.com/thekrauss/kubemanager/internal/modules/insights/service"
)

// Authorizer : droits de la personne sur le projet (iam.Engine).
type Authorizer interface {
	TestPermissions(ctx context.Context, p iam.Principal, r iam.ResourceName, perms []iam.Permission) ([]iam.Permission, error)
}

// Controller : GET /projects/:id/insights.
type Controller struct {
	Service *service.Service
	IAM     Authorizer
}

// sourcePermissions : permissions qui ouvrent chaque source (lectures).
var sourcePermissions = []iam.Permission{
	iam.WorkloadsList, iam.BuildsList, iam.BillingAccountsGet, iam.WorkloadDomainsList,
	iam.SitesList, iam.SiteDomainsList, iam.DatabasesList, iam.AddonsList,
}

// Get : constats du projet, limités à ce que la personne peut lire.
func (h *Controller) Get(c *gin.Context, in *domain.ProjectPath) (*domain.InsightList, error) {
	project, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}
	access, err := h.access(c, project)
	if err != nil {
		return nil, err
	}
	return h.Service.List(c.Request.Context(), project, access)
}

// access : sources accordées, lues sur le projet (la route a déjà vérifié
// la lecture du projet).
func (h *Controller) access(c *gin.Context, project uuid.UUID) (service.Access, error) {
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return service.Access{}, betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	granted, err := h.IAM.TestPermissions(c.Request.Context(), principal, iam.ProjectName(project), sourcePermissions)
	if errors.Is(err, iam.ErrResourceNotFound) {
		return service.Access{}, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}
	if err != nil {
		return service.Access{}, err
	}
	has := map[iam.Permission]bool{}
	for _, p := range granted {
		has[p] = true
	}
	return service.Access{
		Workloads:       has[iam.WorkloadsList],
		Builds:          has[iam.BuildsList],
		Budget:          has[iam.BillingAccountsGet],
		WorkloadDomains: has[iam.WorkloadsList] && has[iam.WorkloadDomainsList],
		SiteDomains:     has[iam.SitesList] && has[iam.SiteDomainsList],
		Databases:       has[iam.DatabasesList],
		Addons:          has[iam.AddonsList],
	}, nil
}
