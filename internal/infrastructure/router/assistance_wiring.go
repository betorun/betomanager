package router

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/assistance"
)

// Accès d'assistance (ADR 0017) : l'équipe demande depuis un ticket (ou
// ouvre en bris de glace), le client accorde, refuse ou retire.

func (a *App) newAssistanceController() *assistance.Controller {
	if a.DB == nil {
		return nil
	}
	s := &assistance.Service{DB: a.DB, Audit: &audit.GormWriter{DB: a.DB}}
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		s.Notify = n
	}
	a.goBackground("assistance-reaper", func(ctx context.Context) {
		s.RunReaper(ctx, func(err error) { a.Logger.Errorw("assistance reaper", "error", err) })
	})
	return &assistance.Controller{Service: s}
}

func addAssistanceRoutes(app *App) {
	r := app.Controllers.Assistance
	if r == nil {
		return
	}
	g := newGuards(app)

	org := RootGroup.NewGroup("/organizations/:id/assistance", "Accès d'assistance de l'équipe betorun").
		Named("Accès d'assistance", "Accès temporaires de l'équipe betorun à l'organisation : demandes, accès ouverts, révocation.")
	org.AddRoute("", http.MethodGet, "Accès de l'équipe betorun sur l'organisation : demandes, accès ouverts, passés",
		g.Perm(iam.OrganizationsGetIamPolicy, security.OrganizationParam("id")), tonic.Handler(r.List, http.StatusOK))
	// Accorder ouvre l'organisation à une personne de l'équipe : double
	// authentification récente (permission sensible).
	grant := g.Perm(iam.OrganizationsGrantAssistance, security.OrganizationParam("id"))
	org.AddRoute("/:grantID/approve", http.MethodPost, "Accepter une demande d'accès de l'équipe (durée de 1 à 24 h)", grant, tonic.Handler(r.Approve, http.StatusOK))
	org.AddRoute("/tickets/:ticketID", http.MethodPost, "Ouvrir un accès à l'agent assigné à un ticket, sans attendre sa demande", grant, tonic.Handler(r.GrantForTicket, http.StatusCreated))
	// Retirer reste immédiat : pas de double authentification.
	revoke := g.Perm(iam.OrganizationsRevokeAssistance, security.OrganizationParam("id"))
	org.AddRoute("/:grantID/deny", http.MethodPost, "Refuser une demande d'accès de l'équipe", revoke, tonic.Handler(r.Deny, http.StatusOK))
	org.AddRoute("/:grantID/revoke", http.MethodPost, "Retirer un accès de l'équipe (effet immédiat)", revoke, tonic.Handler(r.Revoke, http.StatusOK))

	// Équipe : depuis un ticket ; motif exigé et accès inscrit au journal du client.
	tickets := RootGroup.NewGroup("/admin/support/tickets/:ticketID/assistance", "Admin — accès d'assistance").Internal()
	onTicket := staffAssistanceTicket(r)
	tickets.AddRoute("", http.MethodPost, "Demander au client un accès d'assistance (il accepte depuis sa console)",
		g.Perm(iam.PlatformAssistanceRequest, security.PlatformResource).Staff(onTicket), tonic.Handler(r.Request, http.StatusCreated))
	tickets.AddRoute("/breakglass", http.MethodPost, "Bris de glace : accès d'1 h au plus sans l'accord du client (incident ; propriétaires prévenus)",
		g.Perm(iam.PlatformAssistanceBreakglass, security.PlatformResource).Staff(onTicket), tonic.Handler(r.BreakGlass, http.StatusCreated))
	admin := RootGroup.NewGroup("/admin/assistance", "Admin — accès d'assistance").Internal()
	admin.AddRoute("", http.MethodGet, "Mes accès d'assistance (ouverts d'abord)",
		g.Perm(iam.PlatformSupportGet, security.PlatformResource), tonic.Handler(r.Mine, http.StatusOK))
	admin.AddRoute("/:grantID/close", http.MethodPost, "Clore son accès d'assistance, ou retirer sa demande",
		g.Perm(iam.PlatformAssistanceRequest, security.PlatformResource).Staff(staffAssistanceGrant(r)), tonic.Handler(r.Close, http.StatusOK))
}

// staffAssistanceTicket : organisation du ticket (journal du client).
func staffAssistanceTicket(r *assistance.Controller) security.StaffTarget {
	return func(c *gin.Context) (iam.ResourceName, uuid.UUID, error) {
		id, err := uuid.Parse(strings.TrimSpace(c.Param("ticketID")))
		if err != nil {
			return "", uuid.Nil, security.ErrResourceRefInvalid
		}
		org, err := r.Service.TicketOrganization(c.Request.Context(), id)
		if err != nil {
			return "", uuid.Nil, err
		}
		return iam.OrganizationName(org), uuid.Nil, nil
	}
}

// staffAssistanceGrant : organisation d'un accès.
func staffAssistanceGrant(r *assistance.Controller) security.StaffTarget {
	return func(c *gin.Context) (iam.ResourceName, uuid.UUID, error) {
		id, err := uuid.Parse(strings.TrimSpace(c.Param("grantID")))
		if err != nil {
			return "", uuid.Nil, security.ErrResourceRefInvalid
		}
		org, err := r.Service.GrantOrganization(c.Request.Context(), id)
		if err != nil {
			return "", uuid.Nil, err
		}
		return iam.OrganizationName(org), uuid.Nil, nil
	}
}
