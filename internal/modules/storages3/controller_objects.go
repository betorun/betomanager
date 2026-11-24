package storages3

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// multipartOverhead : en-têtes du corps multipart au-delà du fichier.
const multipartOverhead = 1 << 20

func (h *Controller) fail(c *gin.Context, err error) {
	err = apiError(err)
	if h.WriteError != nil {
		h.WriteError(c, err)
		return
	}
	status, body := betoerrors.ToHTTPError(err)
	c.Data(status, "application/json; charset=utf-8", body)
}

func (h *Controller) SetPublic(c *gin.Context, in *domain.SetPublicRequest) (*domain.BucketView, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.SetPublic(c.Request.Context(), p, b, in.Public)
	return out, apiError(err)
}

// RotateKey : nouveau secret ; pour un branchement, il faut aussi le droit
// de modifier les variables du conteneur.
func (h *Controller) RotateKey(c *gin.Context, in *domain.KeyPath) (*domain.CreatedKey, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	keyID, err := uuid.Parse(in.KeyID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrKeyNotFound.Error())
	}
	k, err := h.Service.Key(c.Request.Context(), p, b, keyID)
	if err != nil {
		return nil, apiError(err)
	}
	if k.WorkloadID != nil {
		if err := h.canSetVariables(c, p, *k.WorkloadID); err != nil {
			return nil, err
		}
	}
	out, err := h.Service.RotateKey(c.Request.Context(), p, b, keyID)
	return out, apiError(err)
}

func (h *Controller) ListObjects(c *gin.Context, in *domain.ListObjectsRequest) (*domain.ObjectPage, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.ListObjects(c.Request.Context(), p, b, in.Prefix, in.After, in.Limit)
	return out, apiError(err)
}

func (h *Controller) DeleteObjects(c *gin.Context, in *domain.DeleteObjectsRequest) (*domain.DeleteObjectsResponse, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.DeleteObjects(c.Request.Context(), p, b, in.Keys)
	return out, apiError(err)
}

func (h *Controller) ShareLink(c *gin.Context, in *domain.ShareLinkRequest) (*domain.ShareLink, error) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.ShareLink(c.Request.Context(), p, b, in.Key, in.ExpiresHours)
	return out, apiError(err)
}

// Download : contenu d'un fichier, toujours proposé au téléchargement
// (jamais affiché sous l'adresse de l'API).
func (h *Controller) Download(c *gin.Context, in *domain.ObjectRequest) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		h.fail(c, err)
		return
	}
	rc, obj, err := h.Service.OpenObject(c.Request.Context(), p, b, in.Key)
	if err != nil {
		h.fail(c, err)
		return
	}
	defer rc.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(obj.Key)}))
	c.Header("Content-Length", strconv.FormatInt(obj.Size, 10))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// Upload : un fichier (multipart, champ file), 100 Mo au plus.
func (h *Controller) Upload(c *gin.Context, in *domain.UploadObjectRequest) {
	p, b, err := ids(in.ProjectID, in.BucketID)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, domain.MaxConsoleUpload+multipartOverhead)
	if strings.HasSuffix(in.Key, "/") {
		// Nouveau dossier : pas de fichier.
		out, err := h.Service.PutObject(c.Request.Context(), p, b, in.Key, strings.NewReader(""), 0, "")
		if err != nil {
			h.fail(c, err)
			return
		}
		c.JSON(http.StatusCreated, out)
		return
	}
	fh, err := c.FormFile(domain.ObjectFormField)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.fail(c, domain.ErrObjectTooLarge)
			return
		}
		h.fail(c, betoerrors.New(betoerrors.CodeInvalidInput, `missing "file" field (multipart)`))
		return
	}
	if c.Request.MultipartForm != nil {
		defer func() { _ = c.Request.MultipartForm.RemoveAll() }()
	}
	f, err := fh.Open()
	if err != nil {
		h.fail(c, err)
		return
	}
	defer f.Close()
	ct := fh.Header.Get("Content-Type")
	if byExt := mime.TypeByExtension(path.Ext(in.Key)); byExt != "" {
		ct = byExt
	}
	out, err := h.Service.PutObject(c.Request.Context(), p, b, in.Key, f, fh.Size, ct)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}
