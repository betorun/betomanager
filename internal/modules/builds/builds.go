// Package builds : API des builds d'images et du registre.
package builds

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/core/ginhttp"
	"github.com/thekrauss/kubemanager/internal/core/httpheader"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	"github.com/thekrauss/kubemanager/internal/modules/builds/service"
)

const (
	multipartOverhead = 1 << 20
	fieldName         = "name"
	fieldDockerfile   = "dockerfile"
)

// Controller : routes des builds.
type Controller struct {
	Service *service.Service
	Logger  *zap.SugaredLogger
}

func creator(c *gin.Context) *uuid.UUID {
	if uid, ok := security.InternalUserID(c); ok {
		return &uid
	}
	return nil
}

func parseProject(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	return id, nil
}

func parsePath(in *domain.BuildPath) (uuid.UUID, uuid.UUID, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, err := uuid.Parse(in.BuildID)
	if err != nil {
		return uuid.Nil, uuid.Nil, domain.ErrBuildNotFound
	}
	return project, id, nil
}

// CreateFromGit : build d'un dépôt public.
func (h *Controller) CreateFromGit(c *gin.Context, in *domain.CreateGitBuildRequest) (*domain.Build, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	return h.Service.FromGit(c.Request.Context(), project, *in, creator(c))
}

// Upload : build d'une archive .tar.gz (multipart, hors tonic).
func (h *Controller) Upload(c *gin.Context, in *domain.UploadPath) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	max := h.Service.MaxArchiveBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max+multipartOverhead)
	defer ginhttp.RemoveMultipart(c)
	fh, err := c.FormFile(domain.ArchiveFormField)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.writeError(c, betoerrors.Newf(betoerrors.CodeInvalidInput, "archive too large (maximum %d MB)", max/(1<<20)))
			return
		}
		h.writeError(c, domain.ErrInvalidArchive)
		return
	}
	f, err := fh.Open()
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer f.Close()
	b, err := h.Service.FromArchive(c.Request.Context(), project, c.PostForm(fieldName), c.PostForm(fieldDockerfile), creator(c), f, fh.Size)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, b)
}

// List : builds récents.
func (h *Controller) List(c *gin.Context, in *domain.ListBuildsRequest) ([]domain.Build, error) {
	project, err := parseProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	f, err := buildFilter(in)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Search(c.Request.Context(), project, f)
	if err != nil {
		return nil, err
	}
	// Page pleine : il y en a peut-être d'autres. La réponse reste une liste
	// (CLI, Copilot) ; le curseur suivant passe par un en-tête.
	if len(out) == f.Limit && len(out) > 0 {
		last := out[len(out)-1]
		c.Header("X-Next-Cursor", encodeCursor(last.CreatedAt, last.ID))
	}
	return out, nil
}

func buildFilter(in *domain.ListBuildsRequest) (domain.BuildFilter, error) {
	f := domain.BuildFilter{Name: in.Name, Query: strings.TrimSpace(in.Query), Limit: in.Limit}
	if f.Limit <= 0 || f.Limit > domain.MaxListLimit {
		f.Limit = domain.DefaultListLimit
	}
	if len(f.Query) > 100 {
		return f, domain.ErrInvalidFilter
	}
	switch in.Status {
	case "", domain.FilterActive, domain.StatusQueued, domain.StatusBuilding, domain.StatusSucceeded, domain.StatusFailed, domain.StatusCancelled:
		f.Status = in.Status
	default:
		return f, domain.ErrInvalidFilter
	}
	switch in.Image {
	case "", domain.FilterImageAvailable, domain.FilterImageRemoved:
		f.Image = in.Image
	default:
		return f, domain.ErrInvalidFilter
	}
	if in.Before != "" {
		at, id, err := decodeCursor(in.Before)
		if err != nil {
			return f, domain.ErrInvalidFilter
		}
		f.BeforeAt, f.BeforeID = &at, id
	}
	return f, nil
}

// Curseur : « <créé le en nanosecondes Unix>_<ID> ».
func encodeCursor(at time.Time, id uuid.UUID) string {
	return strconv.FormatInt(at.UnixNano(), 10) + "_" + id.String()
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	ns, rawID, ok := strings.Cut(s, "_")
	if !ok {
		return time.Time{}, uuid.Nil, domain.ErrInvalidFilter
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return time.Unix(0, n).UTC(), id, nil
}

// Get : détails d'un build.
func (h *Controller) Get(c *gin.Context, in *domain.BuildPath) (*domain.Build, error) {
	project, id, err := parsePath(in)
	if err != nil {
		return nil, err
	}
	return h.Service.Get(c.Request.Context(), project, id)
}

// Cancel : arrête un build.
func (h *Controller) Cancel(c *gin.Context, in *domain.BuildPath) error {
	project, id, err := parsePath(in)
	if err != nil {
		return err
	}
	return h.Service.Cancel(c.Request.Context(), project, id)
}

// DeleteImage : retire l'image d'un build.
func (h *Controller) DeleteImage(c *gin.Context, in *domain.BuildPath) error {
	project, id, err := parsePath(in)
	if err != nil {
		return err
	}
	return h.Service.DeleteImage(c.Request.Context(), project, id)
}

// Logs : journal, en flux.
func (h *Controller) Logs(c *gin.Context, in *domain.LogsRequest) {
	project, id, err := parsePath(&domain.BuildPath{ProjectID: in.ProjectID, BuildID: in.BuildID})
	if err != nil {
		h.writeError(c, err)
		return
	}
	stream, err := h.Service.Logs(c.Request.Context(), project, id, in.Follow)
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer stream.Close()
	c.Header(httpheader.ContentType, "text/plain; charset=utf-8")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	ginhttp.CopyFlushing(c, stream)
}

// Source : archive pour le Job de build, protégée par une URL signée.
func (h *Controller) Source(c *gin.Context, in *domain.SourcePath) {
	id, err := uuid.Parse(in.BuildID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	obj, size, err := h.Service.Source(c.Request.Context(), id, in.Expires, in.Sig)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer obj.Close()
	c.DataFromReader(http.StatusOK, size, "application/gzip", obj, nil)
}

// RegistryToken : authentification déléguée par le registre (Docker token auth).
func (h *Controller) RegistryToken(c *gin.Context) {
	user, password, ok := c.Request.BasicAuth()
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="betorun registry"`)
		c.JSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": domain.ErrRegistryForbidden.Error(), "error": domain.ErrRegistryForbidden.Error()})
		return
	}
	res, err := h.Service.RegistryToken(user, password, c.Query("service"), c.QueryArray("scope"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, domain.ErrRegistryForbidden) {
			status = http.StatusUnauthorized
		} else {
			h.Logger.Errorw("registry token", "error", err)
		}
		c.JSON(status, gin.H{"code": "UNAUTHORIZED", "message": err.Error(), "error": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, res)
}

func (h *Controller) writeError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	code := betoerrors.CodeInvalidInput
	switch {
	case errors.Is(err, domain.ErrBuildNotFound):
		status, code = http.StatusNotFound, betoerrors.CodeNotFound
	case errors.Is(err, domain.ErrTooManyBuilds):
		status, code = http.StatusTooManyRequests, "TOO_MANY_BUILDS"
	case errors.Is(err, domain.ErrImageQuota):
		status, code = http.StatusConflict, "IMAGE_QUOTA"
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidPath), errors.Is(err, domain.ErrInvalidArchive):
	default:
		var be *betoerrors.Error
		if errors.As(err, &be) {
			s, body := betoerrors.ToHTTPError(err)
			c.Data(s, httpheader.MIMEJSON, body)
			return
		}
		h.Logger.Errorw("build request failed", "error", err)
		status, code = http.StatusInternalServerError, betoerrors.CodeInternal
		err = errors.New("internal error")
	}
	c.JSON(status, gin.H{"code": code, "message": err.Error(), "error": err.Error()})
}
