package router

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	realtimeCtrl "github.com/thekrauss/kubemanager/internal/modules/realtime"
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
	s := &supportSvc.Service{Store: store, Staff: store, Logger: a.Logger.With("component", "support")}
	if billing := a.billingService(); billing != nil {
		// Contexte d'un ticket : solde et état du porte-monnaie du client.
		s.Wallet = func(ctx context.Context, org uuid.UUID) (*supportSvc.BillingContext, error) {
			w, err := billing.Wallet(ctx, org)
			if err != nil {
				return nil, err
			}
			return &supportSvc.BillingContext{Balance: w.Balance, Currency: w.Currency, Status: w.Status}, nil
		}
	}
	if a.Publisher != nil {
		s.Live = a.Publisher // support:ticket:<ticket>, après chaque changement
	}
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
	// Un ticket précis d'un client : motif exigé, accès visible dans son journal d'audit.
	onTicket := g.Perm(iam.PlatformSupportManage, security.PlatformResource).Staff(staffTicket(r))
	admin.AddRoute("/summary", http.MethodGet, "Compteurs du menu : ouverts, en cours, en attente du client, prioritaires", manage, tonic.Handler(r.Summary, http.StatusOK))
	admin.AddRoute("/tickets", http.MethodGet, "Tickets de tous les clients, à traiter d'abord", manage, tonic.Handler(r.AdminList, http.StatusOK))
	admin.AddRoute("/agents", http.MethodGet, "Membres de l'équipe, pour l'assignation", manage, tonic.Handler(r.Agents, http.StatusOK))
	// Action groupée : un motif commun ; chaque changement est inscrit dans le
	// journal de son ticket, comme une action unitaire.
	admin.AddRoute("/tickets/bulk", http.MethodPost, "Même changement (statut, priorité, agent) sur 50 tickets au plus",
		manage.Staff(staffPlatform), tonic.Handler(r.Bulk, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID", http.MethodGet, "Un ticket, avec ses messages, notes internes, pièces jointes et événements", onTicket, tonic.Handler(r.AdminGet, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/context", http.MethodGet, "Le client derrière un ticket : organisation, porte-monnaie, projets, ressource, autres tickets",
		onTicket, tonic.Handler(r.TicketContext, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/notes", http.MethodPost, "Note interne de l'équipe (jamais montrée au client, aucun e-mail)", onTicket, tonic.Handler(r.AddNote, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID", http.MethodPatch, "Changer le statut, la priorité, la catégorie ou les étiquettes", onTicket, tonic.Handler(r.AdminUpdate, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/assign", http.MethodPost, "Assigner un ticket à un agent", onTicket, tonic.Handler(r.Assign, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/messages", http.MethodPost, "Répondre au client (le ticket attend le client, prévenu par e-mail)", onTicket, tonic.Handler(r.AdminReply, http.StatusOK))
	admin.AddRoute("/tickets/:ticketID/attachments/:attachmentID", http.MethodGet, "Télécharger une pièce jointe", onTicket, wrapRawHandler(r.AdminDownload))
	// En direct (canal support:ticket:<ticket>) : le back-office n'a pas les
	// routes /realtime ; le jeton d'un ticket porte la même garde que sa lecture.
	if rt, ok := app.Controllers.Realtime.(*realtimeCtrl.Controller); ok && rt != nil {
		admin.AddRoute("/live/token", http.MethodGet, "Jeton de connexion au flux en direct du support (back-office)", manage, tonic.Handler(rt.GetConnectionToken, http.StatusOK))
		admin.AddRoute("/tickets/:ticketID/live", http.MethodGet, "Jeton d'abonnement aux changements d'un ticket (back-office)", onTicket, tonic.Handler(rt.GetStaffTicketToken, http.StatusOK))
		rt.TicketOrganization = func(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
			t, err := r.Service.Store.Ticket(ctx, id)
			if err != nil {
				return uuid.Nil, err
			}
			return t.OrganizationID, nil
		}
	}
	admin.AddRoute("/macros", http.MethodGet, "Réponses types", manage, tonic.Handler(r.Macros, http.StatusOK))
	admin.AddRoute("/macros", http.MethodPost, "Créer une réponse type", manage, tonic.Handler(r.CreateMacro, http.StatusCreated))
	admin.AddRoute("/macros/:macroID", http.MethodPatch, "Modifier une réponse type", manage, tonic.Handler(r.UpdateMacro, http.StatusOK))
	admin.AddRoute("/macros/:macroID", http.MethodDelete, "Supprimer une réponse type", manage, tonic.Handler(r.DeleteMacro, http.StatusNoContent))
}

// staffPlatform : action du staff sur plusieurs clients à la fois (motif
// exigé, accès inscrit au journal de la plateforme).
func staffPlatform(*gin.Context) (iam.ResourceName, uuid.UUID, error) {
	return iam.Platform, uuid.Nil, nil
}

// staffTicket : organisation du ticket visé par une route du staff.
func staffTicket(r *support.Controller) security.StaffTarget {
	return func(c *gin.Context) (iam.ResourceName, uuid.UUID, error) {
		id, err := uuid.Parse(strings.TrimSpace(c.Param("ticketID")))
		if err != nil || r == nil || r.Service == nil {
			return "", uuid.Nil, security.ErrResourceRefInvalid
		}
		t, err := r.Service.Store.Ticket(c.Request.Context(), id)
		if err != nil {
			return "", uuid.Nil, err
		}
		project := uuid.Nil
		if t.ProjectID != nil {
			project = *t.ProjectID
		}
		return iam.OrganizationName(t.OrganizationID), project, nil
	}
}
