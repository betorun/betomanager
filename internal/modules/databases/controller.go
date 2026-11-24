package databases

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// Authorizer : décision IAM (iam.Engine).
type Authorizer interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
}

// Controller : routes /projects/:id/databases.
type Controller struct {
	Service *service.Service
	// IAM : second contrôle de l'attache (variables du conteneur).
	IAM Authorizer
}

func NewController(s *service.Service) *Controller { return &Controller{Service: s} }

// apiError : erreurs du service → statut HTTP et message stable.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidStorage), errors.Is(err, domain.ErrInvalidVariable):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, "database not found")
	case errors.Is(err, domain.ErrWorkloadNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrInvalidTarget):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNoBackup):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	case errors.Is(err, domain.ErrNotReady), errors.Is(err, domain.ErrStopped), errors.Is(err, domain.ErrNotStopped):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrNameTaken):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrNotADump), errors.Is(err, domain.ErrDumpTooLarge):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrImportNotFound):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, domain.ErrImportRunning):
		return betoerrors.New(betoerrors.CodeConflict, err.Error())
	case errors.Is(err, domain.ErrImportsDisabled), errors.Is(err, domain.ErrShellDisabled):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case errors.Is(err, domain.ErrConfirmMismatch), errors.Is(err, domain.ErrDumpUnreadable):
		return betoerrors.New(betoerrors.CodeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrNoSnapshot):
		return betoerrors.New(betoerrors.CodeNotFound, err.Error())
	case errors.Is(err, repository.ErrTaskTimeout):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case errors.As(err, new(*repository.TaskError)):
		// Journal de psql ou pg_dump : la base est celle du projet.
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	case errors.Is(err, domain.ErrBackupsUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return betoerrors.New(betoerrors.CodeServiceUnavailable, err.Error())
	case strings.HasPrefix(err.Error(), "at most "), strings.HasPrefix(err.Error(), "project is not ready"):
		return betoerrors.New(betoerrors.CodeUnprocessable, err.Error())
	}
	return err
}

func ids(project, db string) (uuid.UUID, uuid.UUID, error) {
	p, err := uuid.Parse(project)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid project id")
	}
	if db == "" {
		return p, uuid.Nil, nil
	}
	d, err := uuid.Parse(db)
	if err != nil {
		return uuid.Nil, uuid.Nil, betoerrors.New(betoerrors.CodeNotFound, "database not found")
	}
	return p, d, nil
}

func (h *Controller) List(c *gin.Context, in *domain.ProjectPath) ([]domain.DatabaseView, error) {
	p, _, err := ids(in.ProjectID, "")
	if err != nil {
		return nil, err
	}
	out, err := h.Service.List(c.Request.Context(), p)
	return out, apiError(err)
}

func (h *Controller) Get(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Get(c.Request.Context(), p, d)
	return out, apiError(err)
}

func (h *Controller) Create(c *gin.Context, in *domain.CreateDatabaseRequest) (*domain.DatabaseView, error) {
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

func (h *Controller) Delete(c *gin.Context, in *domain.DatabasePath) error {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return err
	}
	return apiError(h.Service.Delete(c.Request.Context(), p, d))
}

// Stop : arrête la base (disque gardé).
func (h *Controller) Stop(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Stop(c.Request.Context(), p, d)
	return out, apiError(err)
}

// EnableBackups : sauvegardes d'une base créée sans elles.
func (h *Controller) EnableBackups(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.EnableBackups(c.Request.Context(), p, d)
	return out, apiError(err)
}

// Start : relance une base arrêtée.
func (h *Controller) Start(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Start(c.Request.Context(), p, d)
	return out, apiError(err)
}

// Attach : la route a vérifié databases.databases.connect sur la base ; il
// faut AUSSI pouvoir modifier les variables du conteneur visé (sinon un
// développeur d'un projet pourrait écrire dans un conteneur qu'il ne gère pas).
func (h *Controller) Attach(c *gin.Context, in *domain.AttachRequest) (*domain.AttachResponse, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
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
	res := iam.ProjectName(p).Child("workloads", w)
	dec, err := h.IAM.Check(c.Request.Context(), principal, iam.WorkloadVariablesUpdate, res)
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return nil, err
	}
	if err != nil || !dec.Allowed {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied: workloads.variables.update on this container")
	}
	out, err := h.Service.Attach(c.Request.Context(), p, d, w, strings.TrimSpace(in.Variable))
	return out, apiError(err)
}

// Restore : la route a vérifié databases.databases.create sur le projet ; il
// faut AUSSI pouvoir lire la base source (la restauration en copie les données).
func (h *Controller) Restore(c *gin.Context, in *domain.RestoreRequest) (*domain.DatabaseView, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	dec, err := h.IAM.Check(c.Request.Context(), principal, iam.DatabasesGet, iam.ProjectName(p).Child("databases", d))
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return nil, err
	}
	if err != nil || !dec.Allowed {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "permission denied: databases.databases.get on the source database")
	}
	out, err := h.Service.Restore(c.Request.Context(), p, d, in.Name, in.TargetTime, string(principal))
	return out, apiError(err)
}

//  Import d'un dump

// multipartOverhead : marge de l'enveloppe multipart autour du fichier.
const multipartOverhead = 1 << 20

// Import : envoi d'un dump (multipart : dump, clean=true pour remplacer les
// objets existants). Hors tonic : le fichier est lu en flux.
func (h *Controller) Import(c *gin.Context, in *domain.ImportUploadPath) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	max := h.Service.MaxDumpBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max+multipartOverhead)
	fh, err := c.FormFile(domain.DumpFormField)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.writeError(c, betoerrors.New(betoerrors.CodeInvalidInput, domain.ErrDumpTooLarge.Error()))
			return
		}
		h.writeError(c, betoerrors.New(betoerrors.CodeInvalidInput, `missing "dump" file (pg_dump -Fc)`))
		return
	}
	if c.Request.MultipartForm != nil {
		defer func() { _ = c.Request.MultipartForm.RemoveAll() }()
	}
	f, err := fh.Open()
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer f.Close()
	creator := ""
	if principal, ok := security.PrincipalFromContext(c); ok {
		creator = string(principal)
	}
	clean := strings.EqualFold(c.PostForm("clean"), "true")
	reset := strings.EqualFold(c.PostForm("reset"), "true")
	// « Vider d'abord » : il faut AUSSI le droit de vider la base.
	if reset {
		if err := h.allowed(c, iam.DatabasesReset, iam.ProjectName(p).Child("databases", d),
			"permission denied: databases.databases.reset on this database"); err != nil {
			h.writeError(c, err)
			return
		}
	}
	imp, err := h.Service.Import(c.Request.Context(), p, d, f, fh.Size, clean, reset, creator)
	if err != nil {
		h.writeError(c, apiError(err))
		return
	}
	c.JSON(http.StatusAccepted, imp)
}

// ListImports : imports récents d'une base.
func (h *Controller) ListImports(c *gin.Context, in *domain.DatabasePath) ([]domain.DatabaseImport, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.ListImports(c.Request.Context(), p, d)
	return out, apiError(err)
}

// GetImport : un import, état à jour.
func (h *Controller) GetImport(c *gin.Context, in *domain.ImportPath) (*domain.DatabaseImport, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.ImportID)
	if err != nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, domain.ErrImportNotFound.Error())
	}
	out, err := h.Service.GetImport(c.Request.Context(), p, d, id)
	return out, apiError(err)
}

// Dump : fichier d'un import, lu par le Job (URL signée, sans session).
func (h *Controller) Dump(c *gin.Context, in *domain.DumpPath) {
	id, err := uuid.Parse(in.ImportID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	obj, size, err := h.Service.Dump(c.Request.Context(), id, in.Expires, in.Sig)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer obj.Close()
	c.DataFromReader(http.StatusOK, size, "application/octet-stream", obj, nil)
}

func (h *Controller) writeError(c *gin.Context, err error) {
	status, body := betoerrors.ToHTTPError(err)
	if status >= http.StatusInternalServerError && h.Service.Logger != nil {
		h.Service.Logger.Errorw("database import failed", "error", err)
	}
	c.Data(status, "application/json; charset=utf-8", body)
}

//  Contenu et remise à zéro

// Contents : taille et tables par schéma, lues dans la base.
func (h *Controller) Contents(c *gin.Context, in *domain.DatabasePath) (*domain.Contents, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	out, err := h.Service.Contents(c.Request.Context(), p, d)
	return out, apiError(err)
}

// Inspect : début d'un dump (multipart : dump), comparé à la base. Rien
// n'est écrit ; seuls les premiers Mio sont lus.
func (h *Controller) Inspect(c *gin.Context, in *domain.ImportUploadPath) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, domain.InspectHeadBytes+multipartOverhead)
	fh, err := c.FormFile(domain.DumpFormField)
	if err != nil {
		h.writeError(c, betoerrors.New(betoerrors.CodeInvalidInput, `missing "dump" file (pg_dump -Fc)`))
		return
	}
	if c.Request.MultipartForm != nil {
		defer func() { _ = c.Request.MultipartForm.RemoveAll() }()
	}
	f, err := fh.Open()
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer f.Close()
	out, err := h.Service.Inspect(c.Request.Context(), p, d, f)
	if err != nil {
		h.writeError(c, apiError(err))
		return
	}
	c.JSON(http.StatusOK, out)
}

// Reset : copie de sécurité (24 h), puis la base est vidée.
func (h *Controller) Reset(c *gin.Context, in *domain.ResetRequest) (*domain.ResetResponse, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	actor := ""
	if principal, ok := security.PrincipalFromContext(c); ok {
		actor = string(principal)
	}
	out, err := h.Service.Reset(c.Request.Context(), p, d, in.ConfirmName, actor)
	return out, apiError(err)
}

// UndoReset : la copie de sécurité est réimportée (suivi comme un import).
func (h *Controller) UndoReset(c *gin.Context, in *domain.DatabasePath) (*domain.DatabaseImport, error) {
	p, d, err := ids(in.ProjectID, in.DatabaseID)
	if err != nil {
		return nil, err
	}
	actor := ""
	if principal, ok := security.PrincipalFromContext(c); ok {
		actor = string(principal)
	}
	out, err := h.Service.UndoReset(c.Request.Context(), p, d, actor)
	return out, apiError(err)
}

// UploadSnapshot : copie de sécurité envoyée par le Job de remise à zéro
// (URL signée, sans session ; PUT du fichier brut).
func (h *Controller) UploadSnapshot(c *gin.Context, in *domain.SnapshotUploadPath) {
	id, err := uuid.Parse(in.TaskID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	err = h.Service.ReceiveSnapshot(c.Request.Context(), id, in.Expires, in.Sig, c.Request.Body, c.Request.ContentLength)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, domain.ErrNoSnapshot):
		c.Status(http.StatusNotFound)
	case errors.Is(err, domain.ErrDumpTooLarge):
		c.Status(http.StatusRequestEntityTooLarge)
	default:
		h.writeError(c, err)
	}
}

// allowed : second contrôle IAM, sur une ressource donnée.
func (h *Controller) allowed(c *gin.Context, perm iam.Permission, res iam.ResourceName, denied string) error {
	principal, ok := security.PrincipalFromContext(c)
	if !ok || h.IAM == nil {
		return betoerrors.New(betoerrors.CodeForbidden, "permission denied")
	}
	dec, err := h.IAM.Check(c.Request.Context(), principal, perm, res)
	if err != nil && !errors.Is(err, iam.ErrResourceNotFound) {
		return err
	}
	if err != nil || !dec.Allowed {
		return betoerrors.New(betoerrors.CodeForbidden, denied)
	}
	return nil
}
