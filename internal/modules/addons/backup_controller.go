package addons

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	"github.com/thekrauss/kubemanager/internal/modules/addons/repository"
)

// backupError : erreurs des sauvegardes → statut HTTP.
func backupError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNoBackups), errors.Is(err, domain.ErrConfirmMismatch):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrBackupNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrBackupRunning), errors.Is(err, domain.ErrBackupNotRestorable):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrBackupsUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case errors.Is(err, repository.ErrBackupTimeout):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	}
	return apiError(err)
}

func actor(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

// ListBackups : sauvegardes d'un service.
func (h *Controller) ListBackups(c *gin.Context, in *domain.AddonPath) (*domain.BackupList, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.ListBackups(c.Request.Context(), p, a)
	return out, backupError(err)
}

// CreateBackup : copie tout de suite (en arrière-plan).
func (h *Controller) CreateBackup(c *gin.Context, in *domain.AddonPath) (*domain.Backup, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.StartBackup(c.Request.Context(), p, a, actor(c))
	return out, backupError(err)
}

// RestoreBackup : copie de l'état courant, puis la sauvegarde est rechargée.
func (h *Controller) RestoreBackup(c *gin.Context, in *domain.RestoreRequest) (*domain.Backup, error) {
	p, a, err := ids(in.ProjectID, in.AddonID)
	if err != nil {
		return nil, err
	}
	b, err := uuid.Parse(in.BackupID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrBackupNotFound.Error())
	}
	out, err := h.Service.Restore(c.Request.Context(), p, a, b, in.ConfirmName, actor(c))
	return out, backupError(err)
}

// UploadBackup : copie envoyée par son Job (URL signée, PUT du fichier brut).
func (h *Controller) UploadBackup(c *gin.Context, in *domain.BackupTransferPath) {
	id, err := uuid.Parse(in.TaskID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	err = h.Service.ReceiveBackup(c.Request.Context(), id, in.Expires, in.Sig, c.Request.Body, c.Request.ContentLength)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, domain.ErrBackupNotFound):
		c.Status(http.StatusNotFound)
	case errors.Is(err, domain.ErrBackupTooLarge):
		c.Status(http.StatusRequestEntityTooLarge)
	default:
		if h.Service.Logger != nil {
			h.Service.Logger.Errorw("addon backup upload failed", "error", err)
		}
		c.Status(http.StatusInternalServerError)
	}
}

// DownloadBackup : sauvegarde lue par le Job de restauration (URL signée).
func (h *Controller) DownloadBackup(c *gin.Context, in *domain.BackupTransferPath) {
	id, err := uuid.Parse(in.TaskID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	obj, size, err := h.Service.OpenBackup(c.Request.Context(), id, in.Expires, in.Sig)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer obj.Close()
	c.DataFromReader(http.StatusOK, size, "application/gzip", obj, nil)
}
