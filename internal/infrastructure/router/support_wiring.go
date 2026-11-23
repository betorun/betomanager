package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/support"
	supportRepo "github.com/thekrauss/kubemanager/internal/modules/support/repository"
	supportSvc "github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// Support (docs/modules/support.md) : tickets des clients, traités par
// l'équipe au back-office. Pièces jointes dans le stockage objet (sans lui :
// indisponibles) ; e-mails au client si l'envoi est configuré.

func (a *App) newSupportController() *support.Controller {
	if a.DB == nil {
		return nil
	}
	store := &supportRepo.GormStore{DB: a.DB}
	s := &supportSvc.Service{Store: store, Logger: a.Logger.With("component", "support")}
	if a.ObjectStore != nil {
		s.Files = a.ObjectStore
	}
	if sender, err := a.mailSender(); err != nil {
		a.Logger.Warnw("support e-mails disabled: mail configuration invalid", "error", err)
	} else if sender != nil {
		s.Notify = &supportSvc.MailNotifier{Store: store, Mail: sender, ConsoleURL: a.Config.Frontend.BaseURL,
			Logger: a.Logger.With("component", "support-mail"), Spawner: a.spawner()}
	}
	return &support.Controller{Service: s, WriteError: writeClassifiedError}
}

// writeClassifiedError : erreur d'une route hors tonic, au format habituel.
func writeClassifiedError(c *gin.Context, err error) {
	status, body := classifyError(err)
	c.AbortWithStatusJSON(status, body)
}

// addSupportRoutes : tickets de l'organisation (clients) ; file de l'équipe
// et réponses types (/admin).
func addSupportRoutes(app *App) {
	r := app.Controllers.Support
	if r == nil {
		return
	}
	g := newGuards(app)
	onOrg := func(p iam.Permission) Access { return g.Perm(p, security.OrganizationParam("id")) }
	list, get, create := onOrg(iam.SupportTicketsList), onOrg(iam.SupportTicketsGet), onOrg(iam.SupportTicketsCreate)

	org := RootGroup.NewGroup("/organizations/:id/support", "Support").
		Named("Support client", "Demandes à l'équipe betorun : questions, facturation, abus, demandes (port 25, déblocage d'adresse…).")
	org.AddRoute("/tickets", http.MethodPost, "Ouvrir un ticket", create, tonic.Handler(r.Create, http.StatusCreated))
	org.AddRoute("/tickets", http.MethodGet, "Tickets de l'organisation, activité la plus récente d'abord", list, tonic.Handler(r.List, http.StatusOK))
	org.AddRoute("/tickets/:ticketID", http.MethodGet, "Un ticket, avec ses messages, pièces jointes et événements", get, tonic.Handler(r.Get, http.StatusOK))
	org.AddRoute("/tickets/:ticketID/messages", http.MethodPost, "Répondre à un ticket (un ticket résolu est rouvert, un ticket fermé refuse)",
		create, tonic.Handler(r.Reply, http.StatusOK))
	org.AddRoute("/tickets/:ticketID/attachments", http.MethodPost, "Joindre un fichier (multipart, champ file : PDF, image ou texte, 10 Mo)",
		create, wrapRawHandler(r.Attach)).
		Documented(RawDoc{Input: support.TicketPath{}, Status: http.StatusCreated, Produces: MIMEJSON, Output: supportSvc.TicketView{},
			Multipart: []FormField{
				{Name: support.AttachmentFormField, Description: "Fichier : PDF, JPEG, PNG, WebP ou texte, 10 Mo au plus", File: true, Required: true},
				{Name: "message_id", Description: "Message du ticket auquel rattacher le fichier"},
			}})
	org.AddRoute("/tickets/:ticketID/attachments/:attachmentID", http.MethodGet, "Télécharger une pièce jointe", get, wrapRawHandler(r.Download)).
		Documented(RawDoc{Input: support.AttachmentPath{}, Produces: "application/octet-stream"})
	org.AddRoute("/tickets/:ticketID/resolve", http.MethodPatch, "Marquer un ticket comme résolu", create, tonic.Handler(r.Resolve, http.StatusOK))
	org.AddRoute("/tickets/:ticketID/reopen", http.MethodPatch, "Rouvrir un ticket résolu (un ticket fermé ne se rouvre pas)", create, tonic.Handler(r.Reopen, http.StatusOK))

	admin := RootGroup.NewGroup("/admin/support", "Admin — support").Internal()
	manage := g.Perm(iam.PlatformSupportManage, security.PlatformResource)
	admin.AddRoute("/summary", http.MethodGet, "Compteurs du menu : ouverts, en cours, en attente du client, prioritaires", manage, tonic.Handler(r.Summary, http.StatusOK))
	admin.AddRoute("/tickets", http.MethodGet, "Tickets de tous les clients, à traiter d'abord", manage, tonic.Handler(r.AdminList, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID", http.MethodGet, "Un ticket, avec ses messages, pièces jointes et événements", manage, tonic.Handler(r.AdminGet, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID", http.MethodPatch, "Changer le statut, la priorité, la catégorie ou les étiquettes", manage, tonic.Handler(r.AdminUpdate, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/assign", http.MethodPost, "Assigner un ticket à un agent", manage, tonic.Handler(r.Assign, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/messages", http.MethodPost, "Répondre au client (le ticket attend le client, prévenu par e-mail)", manage, tonic.Handler(r.AdminReply, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/attachments/:attachmentID", http.MethodGet, "Télécharger une pièce jointe", manage, wrapRawHandler(r.AdminDownload))
	admin.AddRoute("/macros", http.MethodGet, "Réponses types", manage, tonic.Handler(r.Macros, http.StatusOK))
	admin.AddRoute("/macros", http.MethodPost, "Créer une réponse type", manage, tonic.Handler(r.CreateMacro, http.StatusCreated))
	admin.AddRoute("/macros/:macroID", http.MethodPatch, "Modifier une réponse type", manage, tonic.Handler(r.UpdateMacro, http.StatusOK))
	admin.AddRoute("/macros/:macroID", http.MethodDelete, "Supprimer une réponse type", manage, tonic.Handler(r.DeleteMacro, http.StatusNoContent))
}
