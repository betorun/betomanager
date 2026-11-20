package diskresize

import (
	"context"
	"errors"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

// ErrManaged : le porteur d'un service s'agrandit depuis la page Services.
var ErrManaged = errors.New("this container is managed by a service: grow its disk from the Services page")

// Addons : service → conteneur porteur et tailles permises par son modèle.
type Addons interface {
	Carrier(ctx context.Context, project, addon uuid.UUID) (workload uuid.UUID, disks []string, err error)
}

// Controller : routes d'agrandissement.
type Controller struct {
	Service *Service
	Addons  Addons
}

// WorkloadPath : /workloads/:id/disk.
type WorkloadPath struct {
	ID string `path:"id" binding:"required"`
}

// ResizeRequest : POST /workloads/:id/disk.
type ResizeRequest struct {
	ID   string `path:"id" binding:"required"`
	Size string `json:"size" description:"Nouvelle taille : 2Gi, 5Gi, 10Gi ou 20Gi, plus grande que l'actuelle (vide : reprendre une opération interrompue)"`
}

// AddonPath : /projects/:id/addons/:addonID/disk.
type AddonPath struct {
	ProjectID string `path:"id" binding:"required"`
	AddonID   string `path:"addonID" binding:"required"`
}

// AddonResizeRequest : POST /projects/:id/addons/:addonID/disk.
type AddonResizeRequest struct {
	ProjectID string `path:"id" binding:"required"`
	AddonID   string `path:"addonID" binding:"required"`
	Size      string `json:"size" description:"Nouvelle taille, parmi celles du modèle (vide : reprendre une opération interrompue)"`
}

// LastResize : réponse des lectures (resize nul : jamais agrandi).
type LastResize struct {
	Resize *Resize  `json:"resize"`
	Sizes  []string `json:"sizes"`
}

func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, ErrInvalidSize), errors.Is(err, ErrNotPersistent), errors.Is(err, ErrQuota), errors.Is(err, ErrResumeSize), errors.Is(err, ErrManaged):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, ErrRunning):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, ErrUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	}
	return err
}

func actor(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

func (h *Controller) workload(ctx context.Context, raw string) (*Workload, error) {
	id, err := uuid.Parse(raw)
	if err != nil || h.Service.Workloads == nil {
		return nil, ErrNotFound
	}
	w, err := h.Service.Workloads.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, ErrNotFound
	}
	return w, nil
}

// sizesAbove : tailles proposées, plus grandes que l'actuelle.
func sizesAbove(all []string, current string) []string {
	out := []string{}
	for _, s := range all {
		if bytesOf(s) > bytesOf(current) {
			out = append(out, s)
		}
	}
	return out
}

// GetWorkload : dernière opération d'un conteneur et tailles proposées.
func (h *Controller) GetWorkload(c *gin.Context, in *WorkloadPath) (*LastResize, error) {
	w, err := h.workload(c.Request.Context(), in.ID)
	if err != nil {
		return nil, apiError(err)
	}
	r, err := h.Service.Last(c.Request.Context(), w.ProjectID, w.ID)
	sizes := []string{}
	if w.Persistent && !w.Managed {
		sizes = sizesAbove(Sizes, w.StorageSize)
	}
	return &LastResize{Resize: r, Sizes: sizes}, apiError(err)
}

// ResizeWorkload : agrandit le disque d'un conteneur (en arrière-plan).
func (h *Controller) ResizeWorkload(c *gin.Context, in *ResizeRequest) (*Resize, error) {
	w, err := h.workload(c.Request.Context(), in.ID)
	if err != nil {
		return nil, apiError(err)
	}
	if w.Managed {
		return nil, apiError(ErrManaged)
	}
	r, err := h.Service.Start(c.Request.Context(), w.ProjectID, w.ID, in.Size, actor(c))
	return r, apiError(err)
}

func (h *Controller) carrier(ctx context.Context, project, addon string) (uuid.UUID, uuid.UUID, []string, error) {
	p, err1 := uuid.Parse(project)
	a, err2 := uuid.Parse(addon)
	if err1 != nil || err2 != nil || h.Addons == nil {
		return uuid.Nil, uuid.Nil, nil, ErrNotFound
	}
	w, disks, err := h.Addons.Carrier(ctx, p, a)
	return p, w, disks, err
}

// GetAddon : dernière opération sur le disque d'un service.
func (h *Controller) GetAddon(c *gin.Context, in *AddonPath) (*LastResize, error) {
	p, wid, disks, err := h.carrier(c.Request.Context(), in.ProjectID, in.AddonID)
	if err != nil {
		return nil, apiError(err)
	}
	w, err := h.workload(c.Request.Context(), wid.String())
	if err != nil {
		return nil, apiError(err)
	}
	r, err := h.Service.Last(c.Request.Context(), p, wid)
	sizes := []string{}
	if w.Persistent {
		sizes = sizesAbove(disks, w.StorageSize)
	}
	return &LastResize{Resize: r, Sizes: sizes}, apiError(err)
}

// ResizeAddon : agrandit le disque d'un service (tailles de son modèle).
func (h *Controller) ResizeAddon(c *gin.Context, in *AddonResizeRequest) (*Resize, error) {
	p, wid, disks, err := h.carrier(c.Request.Context(), in.ProjectID, in.AddonID)
	if err != nil {
		return nil, apiError(err)
	}
	if in.Size != "" && !slices.Contains(disks, in.Size) {
		return nil, apiError(ErrInvalidSize)
	}
	r, err := h.Service.Start(c.Request.Context(), p, wid, in.Size, actor(c))
	return r, apiError(err)
}
