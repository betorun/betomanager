// Package support : tickets de support, routes HTTP (docs/modules/support.md).
// Côté client sous /organizations/:id/support, côté équipe sous /admin/support.
package support

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
	"github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// AttachmentFormField : champ multipart du fichier joint.
const AttachmentFormField = "file"

// multipartOverhead : marge de l'enveloppe multipart autour du fichier.
const multipartOverhead = 1 << 20

// Controller : routes du support.
type Controller struct {
	Service *service.Service
	// WriteError : réponse d'erreur des routes hors tonic (fichiers), même
	// format que les autres.
	WriteError func(c *gin.Context, err error)
}

type TicketResponse = service.TicketView
type ListTicketsResponse = service.TicketList
type StaffTicketResponse = service.StaffTicketView
type StaffTicketsResponse = service.StaffTicketList

// me : personne derrière la requête (nil pour un compte de service).
func me(c *gin.Context) *uuid.UUID { return actor(c).UserID }

func actor(c *gin.Context) service.Actor {
	p, _ := security.PrincipalFromContext(c)
	return service.ActorOf(string(p))
}

func parseOrg(raw string) (uuid.UUID, error) {
	org, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	return org, nil
}

// parseID : un identifiant illisible est introuvable.
func parseID(raw string, notFound error) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, notFound
	}
	return id, nil
}

// ── Client ──────────────────────────────────────────────────────────────

type OrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

type TicketPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	TicketID       string `path:"ticketID" description:"Identifiant du ticket"`
}

func (p TicketPath) ids() (uuid.UUID, uuid.UUID, error) {
	org, err := parseOrg(p.OrganizationID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, err := parseID(p.TicketID, domain.ErrTicketNotFound)
	return org, id, err
}

type CreateTicketInput struct {
	OrgPath
	service.CreateInput
}

func (h *Controller) Create(c *gin.Context, in *CreateTicketInput) (*TicketResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.Create(c.Request.Context(), org, actor(c), in.CreateInput)
}

type ListTicketsInput struct {
	OrgPath
	service.ListInput
}

func (h *Controller) List(c *gin.Context, in *ListTicketsInput) (*ListTicketsResponse, error) {
	org, err := parseOrg(in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return h.Service.List(c.Request.Context(), org, in.ListInput)
}

func (h *Controller) Get(c *gin.Context, in *TicketPath) (*TicketResponse, error) {
	org, id, err := in.ids()
	if err != nil {
		return nil, err
	}
	return h.Service.Get(c.Request.Context(), org, id)
}

type MessageBody struct {
	Body string `json:"body" binding:"required" description:"Message, texte brut (10 000 caractères au plus)"`
}

type ReplyInput struct {
	TicketPath
	MessageBody
}

func (h *Controller) Reply(c *gin.Context, in *ReplyInput) (*TicketResponse, error) {
	org, id, err := in.ids()
	if err != nil {
		return nil, err
	}
	return h.Service.Reply(c.Request.Context(), org, id, actor(c), in.Body)
}

func (h *Controller) Resolve(c *gin.Context, in *TicketPath) (*TicketResponse, error) {
	org, id, err := in.ids()
	if err != nil {
		return nil, err
	}
	return h.Service.Resolve(c.Request.Context(), org, id, actor(c))
}

func (h *Controller) Reopen(c *gin.Context, in *TicketPath) (*TicketResponse, error) {
	org, id, err := in.ids()
	if err != nil {
		return nil, err
	}
	return h.Service.Reopen(c.Request.Context(), org, id, actor(c))
}

// Attach : multipart, champ "file" (10 Mo), "message_id" facultatif. Hors
// tonic : le fichier est lu en flux.
func (h *Controller) Attach(c *gin.Context, in *TicketPath) {
	org, id, err := in.ids()
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, domain.AttachmentMaxSize+multipartOverhead)
	fh, err := c.FormFile(AttachmentFormField)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.fail(c, domain.ErrAttachmentTooLarge)
			return
		}
		h.fail(c, domain.ErrAttachmentMissing)
		return
	}
	if c.Request.MultipartForm != nil {
		defer func() { _ = c.Request.MultipartForm.RemoveAll() }()
	}
	var msgID *uuid.UUID
	if raw := strings.TrimSpace(c.PostForm("message_id")); raw != "" {
		m, err := parseID(raw, domain.ErrMessageNotOnTicket)
		if err != nil {
			h.fail(c, err)
			return
		}
		msgID = &m
	}
	f, err := fh.Open()
	if err != nil {
		h.fail(c, err)
		return
	}
	defer f.Close()
	v, err := h.Service.Attach(c.Request.Context(), org, id, actor(c),
		service.AttachInput{FileName: fh.Filename, Size: fh.Size, Content: f, MessageID: msgID})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}

type AttachmentPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	TicketID       string `path:"ticketID" description:"Identifiant du ticket"`
	AttachmentID   string `path:"attachmentID" description:"Identifiant de la pièce jointe"`
}

// Download : contenu d'une pièce jointe (toujours proposé au téléchargement).
func (h *Controller) Download(c *gin.Context, in *AttachmentPath) {
	org, ticket, err := TicketPath{OrganizationID: in.OrganizationID, TicketID: in.TicketID}.ids()
	if err != nil {
		h.fail(c, err)
		return
	}
	h.serve(c, &org, ticket, in.AttachmentID)
}

// ── Équipe ──────────────────────────────────────────────────────────────

type AdminTicketPath struct {
	TicketID string `path:"ticketID" description:"Identifiant du ticket"`
}

func (p AdminTicketPath) id() (uuid.UUID, error) {
	return parseID(p.TicketID, domain.ErrTicketNotFound)
}

type AdminListInput struct {
	service.AdminListInput
}

func (h *Controller) AdminList(c *gin.Context, in *AdminListInput) (*StaffTicketsResponse, error) {
	return h.Service.AdminList(c.Request.Context(), in.AdminListInput, me(c))
}

func (h *Controller) AdminGet(c *gin.Context, in *AdminTicketPath) (*StaffTicketResponse, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.AdminGet(c.Request.Context(), id)
}

type AdminReplyInput struct {
	AdminTicketPath
	MessageBody
}

func (h *Controller) AdminReply(c *gin.Context, in *AdminReplyInput) (*StaffTicketResponse, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.AdminReply(c.Request.Context(), id, actor(c), in.Body)
}

type AdminUpdateInput struct {
	AdminTicketPath
	service.UpdateInput
}

func (h *Controller) AdminUpdate(c *gin.Context, in *AdminUpdateInput) (*StaffTicketResponse, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.AdminUpdate(c.Request.Context(), id, actor(c), in.UpdateInput)
}

type AssignInput struct {
	AdminTicketPath
	service.AssignInput
}

func (h *Controller) Assign(c *gin.Context, in *AssignInput) (*StaffTicketResponse, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.Assign(c.Request.Context(), id, actor(c), in.AssignedToUserID)
}

type AdminAttachmentPath struct {
	TicketID     string `path:"ticketID" description:"Identifiant du ticket"`
	AttachmentID string `path:"attachmentID" description:"Identifiant de la pièce jointe"`
}

func (h *Controller) AdminDownload(c *gin.Context, in *AdminAttachmentPath) {
	ticket, err := parseID(in.TicketID, domain.ErrTicketNotFound)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.serve(c, nil, ticket, in.AttachmentID)
}

func (h *Controller) Summary(c *gin.Context) (*domain.Summary, error) {
	s, err := h.Service.Summary(c.Request.Context(), me(c))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// TicketContext : le client derrière un ticket.
func (h *Controller) TicketContext(c *gin.Context, in *AdminTicketPath) (*service.TicketContext, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.TicketContext(c.Request.Context(), id)
}

type AgentsResponse struct {
	Items []service.Agent `json:"items"`
}

func (h *Controller) Agents(c *gin.Context) (*AgentsResponse, error) {
	list, err := h.Service.Agents(c.Request.Context())
	if err != nil {
		return nil, err
	}
	return &AgentsResponse{Items: list}, nil
}

func (h *Controller) AddNote(c *gin.Context, in *AdminReplyInput) (*StaffTicketResponse, error) {
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	return h.Service.AddNote(c.Request.Context(), id, actor(c), in.Body)
}

type BulkInput struct {
	service.BulkInput
}

func (h *Controller) Bulk(c *gin.Context, in *BulkInput) (*service.BulkResponse, error) {
	return h.Service.Bulk(c.Request.Context(), actor(c), in.BulkInput)
}

type MacroResponse struct {
	Macro *domain.Macro `json:"macro"`
}

type MacrosResponse struct {
	Items []domain.Macro `json:"items"`
}

func (h *Controller) Macros(c *gin.Context) (*MacrosResponse, error) {
	list, err := h.Service.Macros(c.Request.Context())
	if err != nil {
		return nil, err
	}
	return &MacrosResponse{Items: list}, nil
}

type CreateMacroInput struct {
	service.MacroInput
}

func (h *Controller) CreateMacro(c *gin.Context, in *CreateMacroInput) (*MacroResponse, error) {
	m, err := h.Service.CreateMacro(c.Request.Context(), in.MacroInput)
	if err != nil {
		return nil, err
	}
	return &MacroResponse{Macro: m}, nil
}

type MacroPath struct {
	MacroID string `path:"macroID" description:"Identifiant de la réponse type"`
}

type UpdateMacroInput struct {
	MacroPath
	service.MacroInput
}

func (h *Controller) UpdateMacro(c *gin.Context, in *UpdateMacroInput) (*MacroResponse, error) {
	id, err := parseID(in.MacroID, domain.ErrMacroNotFound)
	if err != nil {
		return nil, err
	}
	m, err := h.Service.UpdateMacro(c.Request.Context(), id, in.MacroInput)
	if err != nil {
		return nil, err
	}
	return &MacroResponse{Macro: m}, nil
}

func (h *Controller) DeleteMacro(c *gin.Context, in *MacroPath) error {
	id, err := parseID(in.MacroID, domain.ErrMacroNotFound)
	if err != nil {
		return err
	}
	return h.Service.DeleteMacro(c.Request.Context(), id)
}

// ── Commun ──────────────────────────────────────────────────────────────

func (h *Controller) serve(c *gin.Context, org *uuid.UUID, ticket uuid.UUID, rawID string) {
	id, err := parseID(rawID, domain.ErrAttachmentNotFound)
	if err != nil {
		h.fail(c, err)
		return
	}
	obj, a, err := h.Service.OpenAttachment(c.Request.Context(), org, ticket, id)
	if err != nil {
		h.fail(c, err)
		return
	}
	defer obj.Close()
	c.Header("Content-Type", a.MimeType)
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.FileName}))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	http.ServeContent(c.Writer, c.Request, "", a.CreatedAt, io.ReadSeeker(obj))
}

func (h *Controller) fail(c *gin.Context, err error) {
	if h.WriteError != nil {
		h.WriteError(c, err)
		return
	}
	status, body := betoerrors.ToHTTPError(err)
	c.Data(status, "application/json; charset=utf-8", body)
}
