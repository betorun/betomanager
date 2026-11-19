package addons

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
)

// Authorizer : décision IAM (iam.Engine).
type Authorizer interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

// Controller : routes /projects/:id/addons.
type Controller struct {
	Service *Service
	// IAM : second contrôle du branchement (variables du conteneur visé).
	IAM Authorizer
}

func NewController(s *Service) *Controller { return &Controller{Service: s} }

// apiError : erreurs du service → statut HTTP et message stable.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrUnknownTemplate), errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidVersion),
		errors.Is(err, domain.ErrInvalidMode), errors.Is(err, domain.ErrInvalidSize), errors.Is(err, domain.ErrInvalidDisk),
		errors.Is(err, domain.ErrInvalidVariable), errors.Is(err, domain.ErrSelfAttach), errors.Is(err, domain.ErrNotAnnounced):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrWorkloadNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrNameTaken), errors.Is(err, domain.ErrNotReady), errors.Is(err, domain.ErrNoUpgrade):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrLimitReached):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case strings.HasPrefix(err.Error(), "project is not ready"):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	}
	return err
}

func ids(project, addon string) (uuid.UUID, uuid.UUID, error) {
	p, err := uuid.Parse(project)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	if addon == "" {
		return p, uuid.Nil, nil
	}
	a, err := uuid.Parse(addon)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrNotFound.Error())
	}
	return p, a, nil
}

// Templates : catalogue (modèles, versions, usages, tailles, disques).
func (h *Controller) Templates(_ *gin.Context, _ *domain.ProjectPath) (*domain.CatalogView, error) {
	return &domain.CatalogView{Templates: h.Service.Catalog(), Sizes: domain.Sizes, Upcoming: domain.Announced}, nil
}

// currentUser : compte de l'appelant (« Me prévenir » est personnel).
func currentUser(c *gin.Context) (uuid.UUID, error) {
	principal, ok := security.PrincipalFromContext(c)
	if !ok {
		return uuid.Nil, betoerrors.New(betoerrors.CodeUnauthorized, "authentication required")
	}
	kind, id, err := principal.Parse()
	if err != nil || kind != iam.KindUser {
		return uuid.Nil, betoerrors.New(betoerrors.CodeForbidden, domain.ErrNotAUser.Error())
	}
	return id, nil
}

// Waitlist : services annoncés pour lesquels l'appelant sera prévenu.
func (h *Controller) Waitlist(c *gin.Context) (*domain.WaitlistView, error) {
	user, err := currentUser(c)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Waitlist(c.Request.Context(), user)
	return out, apiError(err)
}

// JoinWaitlist : être prévenu par e-mail quand le service ouvre.
func (h *Controller) JoinWaitlist(c *gin.Context, in *domain.WaitlistPath) (*domain.WaitlistView, error) {
	user, err := currentUser(c)
	if err != nil {
		return nil, err
	}
	if err := h.Service.JoinWaitlist(c.Request.Context(), user, in.Template); err != nil {
		return nil, apiError(err)
	}
	out, err := h.Service.Waitlist(c.Request.Context(), user)
	return out, apiError(err)
}

// LeaveWaitlist : ne plus être prévenu.
func (h *Controller) LeaveWaitlist(c *gin.Context, in *domain.WaitlistPath) (*domain.WaitlistView, error) {
	user, err := currentUser(c)
	if err != nil {
		return nil, err
	}
	if err := h.Service.LeaveWaitlist(c.Request.Context(), user, in.Template); err != nil {
		return nil, apiError(err)
	}
	out, err := h.Service.Waitlist(c.Request.Context(), user)
	return out, apiError(err)
}

func (h *Controller) List(c *gin.Context, in *domain.ProjectPath) (*domain.AddonList, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	out, err := h.Service.List(c.Request.Context(), p)
	return out, apiError(err)
}

func (h *Controller) Get(c *gin.Context, in *domain.AddonPath) (*domain.AddonView, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Get(c.Request.Context(), p, a)
	return out, apiError(err)
}

func (h *Controller) Create(c *gin.Context, in *domain.CreateAddonRequest) (*domain.AddonView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	creator := ""
	if principal, ok := security.PrincipalFromContext(c); ok {
		creator = string(principal)
	}
	out, err := h.Service.Create(c.Request.Context(), p, *in, creator)
	return out, apiError(err)
}

func (h *Controller) Upgrade(c *gin.Context, in *domain.UpgradeRequest) (*domain.AddonView, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Upgrade(c.Request.Context(), p, a, in.Version)
	return out, apiError(err)
}

func (h *Controller) Delete(c *gin.Context, in *domain.AddonPath) (*domain.DeleteResponse, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Delete(c.Request.Context(), p, a)
	return out, apiError(err)
}

// Attach : la route a vérifié addons.instances.connect sur le service ; il
// faut AUSSI pouvoir modifier les variables du conteneur visé (sinon un
// développeur pourrait écrire dans un conteneur qu'il ne gère pas).
func (h *Controller) Attach(c *gin.Context, in *domain.AttachRequest) (*domain.AttachResponse, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	w, err := uuid.Parse(strings.TrimSpace(in.WorkloadID))
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrWorkloadNotFound.Error())
	}
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	dec, err := h.IAM.Check(c.Request.Context(), principal, iam.WorkloadVariablesUpdate, iam.ProjectName(p).Child("workloads", w))
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return nil, err
	}
	if err != nil || !dec.Allowed {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied: workloads.variables.update on this container")
	}
	out, err := h.Service.Attach(c.Request.Context(), p, a, w, strings.TrimSpace(in.Variable))
	return out, apiError(err)
}
