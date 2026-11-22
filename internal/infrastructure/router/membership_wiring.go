package router

import (
	"context"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	alertsDomain "github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
	alertsSvc "github.com/thekrauss/kubemanager/internal/modules/alerts/service"
)

// membershipMails : e-mail aux personnes dont les accès changent (rôle sur un
// projet ou une organisation, groupe). Implémente iam.MembershipNotifier.
type membershipMails struct {
	app *App
	n   *alertsSvc.Notifier
}

func (a *App) membershipNotifier() iam.MembershipNotifier {
	n := a.alertNotifier()
	if n == nil {
		return nil
	}
	return &membershipMails{app: a, n: n}
}

// person : adresse et nom d'une personne (user:<uuid>) ; ok=false sinon.
func (m *membershipMails) person(ctx context.Context, p iam.Principal) (alertsDomain.Recipient, bool) {
	kind, id, err := p.Parse()
	if err != nil || kind != iam.KindUser {
		return alertsDomain.Recipient{}, false
	}
	var r alertsDomain.Recipient
	row := m.app.DB.WithContext(ctx).Raw(`SELECT id, email, full_name, locale FROM users WHERE id = ? AND deleted_at IS NULL`, id).Row()
	if err := row.Scan(&r.ID, &r.Email, &r.FullName, &r.Locale); err != nil || r.Email == "" {
		return alertsDomain.Recipient{}, false
	}
	return r, true
}

func (m *membershipMails) name(ctx context.Context, table string, id uuid.UUID) string {
	var name string
	col := "name"
	if table == "organizations" {
		col = "display_name"
	}
	_ = m.app.DB.WithContext(ctx).Raw(`SELECT `+col+` FROM `+table+` WHERE id = ?`, id).Row().Scan(&name)
	return name
}

func (m *membershipMails) AccessChanged(ctx context.Context, caller iam.Principal, r iam.ResourceName, c iam.MemberChange) {
	// Projet ou organisation seulement (pas les sous-ressources).
	var resource, name string
	if id, ok := r.Project(); ok && r == iam.ProjectName(id) {
		resource, name = alertsSvc.ResourceProject, m.name(ctx, "projects", id)
	} else if id, ok := r.Organization(); ok && r == iam.OrganizationName(id) {
		resource, name = alertsSvc.ResourceOrganization, m.name(ctx, "organizations", id)
	} else {
		return
	}
	to, ok := m.person(ctx, c.Principal)
	if !ok || name == "" {
		return
	}
	change := alertsSvc.AccessChanged
	switch {
	case len(c.Before) == 0:
		change = alertsSvc.AccessGranted
	case len(c.After) == 0:
		change = alertsSvc.AccessRemoved
	}
	titles := make([]string, 0, len(c.After))
	for _, role := range c.After {
		titles = append(titles, iam.RoleTitle(ctx, m.app.DB, role))
	}
	m.n.AccessChanged(ctx, to, resource, change, name, titles, m.app.principalLabel(ctx, caller))
}

func (m *membershipMails) GroupMembershipChanged(ctx context.Context, caller iam.Principal, g *iam.Group, member iam.Principal, added bool) {
	to, ok := m.person(ctx, member)
	if !ok {
		return
	}
	group := g.DisplayName
	if group == "" {
		group = g.GroupID
	}
	by := ""
	if caller != "" {
		by = m.app.principalLabel(ctx, caller)
	}
	m.n.GroupMembershipChanged(ctx, to, group, m.name(ctx, "organizations", g.OrganizationID), added, by)
}
