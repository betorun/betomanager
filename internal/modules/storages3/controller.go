// Package storages3 : routes /projects/:id/storage/buckets (ADR 0015).
package storages3

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
	"github.com/thekrauss/kubemanager/internal/modules/storages3/service"
)

// Authorizer : décision IAM (iam.Engine).
type Authorizer interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

type Controller struct {
	Service *service.Service
	// IAM : second contrôle du branchement (variables du conteneur).
	IAM Authorizer
	// WriteError : erreurs des routes brutes (fichiers), traduites comme
	// les autres ; nil : JSON simple.
	WriteError func(c *gin.Context, err error)
}

func NewController(s *service.Service) *Controller { return &Controller{Service: s} }

// apiError : erreurs du service → statut HTTP et message stable.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidQuota), errors.Is(err, domain.ErrInvalidPrefix),
		errors.Is(err, domain.ErrInvalidKeyName), errors.Is(err, domain.ErrInvalidAccess), errors.Is(err, domain.ErrInvalidObject),
		errors.Is(err, domain.ErrInvalidExpiry), errors.Is(err, domain.ErrObjectTooLarge):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrKeyNotFound), errors.Is(err, domain.ErrWorkloadNotFound),
		errors.Is(err, domain.ErrObjectNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrNameTaken), errors.Is(err, domain.ErrAlreadyAttached), errors.Is(err, domain.ErrNotEmpty),
		errors.Is(err, domain.ErrDeleting), errors.Is(err, domain.ErrSuspended):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrTooMany), errors.Is(err, domain.ErrTooManyKeys), errors.Is(err, service.ErrProjectNotReady),
		errors.Is(err, domain.ErrOverQuota):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	}
	return err
}

func ids(project, bucket string) (uuid.UUID, uuid.UUID, error) {
	p, err := uuid.Parse(project)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	if bucket == "" {
		return p, uuid.Nil, nil
	}
	b, err := uuid.Parse(bucket)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrNotFound.Error())
	}
	return p, b, nil
}

func actor(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

func (h *Controller) List(c *gin.Context, in *domain.ProjectPath) ([]domain.BucketView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	out, err := h.Service.List(c.Request.Context(), p)
	return out, apiError(err)
}

func (h *Controller) Create(c *gin.Context, in *domain.CreateBucketRequest) (*domain.BucketView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Create(c.Request.Context(), p, *in, actor(c))
	return out, apiError(err)
}

func (h *Controller) Get(c *gin.Context, in *domain.BucketPath) (*domain.BucketView, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Get(c.Request.Context(), p, b)
	return out, apiError(err)
}

func (h *Controller) Update(c *gin.Context, in *domain.UpdateBucketRequest) (*domain.BucketView, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.SetQuota(c.Request.Context(), p, b, in.QuotaGB)
	return out, apiError(err)
}

func (h *Controller) Delete(c *gin.Context, in *domain.DeleteBucketRequest) error {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return err
	}
	return apiError(h.Service.Delete(c.Request.Context(), p, b, in.Confirm))
}

// Attach : il faut aussi le droit de modifier les variables du conteneur.
func (h *Controller) Attach(c *gin.Context, in *domain.AttachRequest) (*domain.AttachResponse, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	w, err := uuid.Parse(strings.TrimSpace(in.WorkloadID))
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrWorkloadNotFound.Error())
	}
	if err := h.canSetVariables(c, p, w); err != nil {
		return nil, err
	}
	out, err := h.Service.Attach(c.Request.Context(), p, b, w, strings.TrimSpace(in.Prefix), strings.TrimSpace(in.Access), actor(c))
	return out, apiError(err)
}

// canSetVariables : brancher ou faire tourner la clé d'un conteneur pose
// ses variables : il faut aussi ce droit-là, sur ce conteneur.
func (h *Controller) canSetVariables(c *gin.Context, project, workload uuid.UUID) error {
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	dec, err := h.IAM.Check(c.Request.Context(), principal, iam.WorkloadVariablesUpdate, iam.ProjectName(project).Child("workloads", workload))
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return err
	}
	if err != nil || !dec.Allowed {
		return betoerrors.New(betoerrors.CodeForbidden, "permission denied: workloads.variables.update on this container")
	}
	return nil
}

func (h *Controller) CreateKey(c *gin.Context, in *domain.CreateKeyRequest) (*domain.CreatedKey, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.CreateKey(c.Request.Context(), p, b, in.Name, strings.TrimSpace(in.Access), actor(c))
	return out, apiError(err)
}

func (h *Controller) DeleteKey(c *gin.Context, in *domain.KeyPath) error {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return err
	}
	k, err := uuid.Parse(in.KeyID)
	if err != nil {
		return betoerrors.New(betoerrors.CodeNotFound, domain.ErrKeyNotFound.Error())
	}
	return apiError(h.Service.DeleteKey(c.Request.Context(), p, b, k))
}
