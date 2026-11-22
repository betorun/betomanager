package service

import (
	"context"
	"strings"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
)

// Accès d'une personne : rôle accordé, modifié ou retiré sur un projet ou
// une organisation, entrée ou sortie d'un groupe. L'e-mail part à la personne
// concernée, à chaque changement (pas de limite de fréquence : c'est un acte
// d'un administrateur).

// Changements d'accès.
const (
	AccessGranted = "granted"
	AccessChanged = "changed"
	AccessRemoved = "removed"
)

// Ressources.
const (
	ResourceProject      = "project"
	ResourceOrganization = "organization"
)

var membershipOutro = map[i18n.Locale]string{
	i18n.FR: "Vous recevez cet e-mail parce que vos accès sur betorun ont changé. Si cela vous surprend, contactez un propriétaire.",
	i18n.EN: "You receive this email because your access on betorun changed. If this surprises you, contact an owner.",
}

// accessCopies : ressource, puis changement. {{.Name}} : nom de la ressource ;
// {{.Roles}} et {{.By}} remplacés avant le rendu.
var accessCopies = map[string]map[string]map[i18n.Locale]alertCopy{
	ResourceProject: {
		AccessGranted: pair(
			alertCopy{"Vous avez accès au projet « {{.Name}} »", "Nouvel accès",
				"Vous avez maintenant accès au projet « {{.Name}} » sur betorun{{.By}}. Rôle : {{.Roles}}.", "Ouvrir la console"},
			alertCopy{"You now have access to project \"{{.Name}}\"", "New access",
				"You now have access to project \"{{.Name}}\" on betorun{{.By}}. Role: {{.Roles}}.", "Open the console"}),
		AccessChanged: pair(
			alertCopy{"Votre rôle sur le projet « {{.Name}} » a changé", "Rôle modifié",
				"Votre rôle sur le projet « {{.Name}} » a été modifié{{.By}}. Rôle : {{.Roles}}.", "Ouvrir la console"},
			alertCopy{"Your role in project \"{{.Name}}\" changed", "Role changed",
				"Your role in project \"{{.Name}}\" was changed{{.By}}. Role: {{.Roles}}.", "Open the console"}),
		AccessRemoved: pair(
			alertCopy{"Votre accès au projet « {{.Name}} » a été retiré", "Accès retiré",
				"Vous n'avez plus accès au projet « {{.Name}} »{{.By}}. Les accès que vous tenez d'une organisation ou d'un groupe restent valables.", "Ouvrir la console"},
			alertCopy{"Your access to project \"{{.Name}}\" was removed", "Access removed",
				"You no longer have access to project \"{{.Name}}\"{{.By}}. Access you hold through an organization or a group still applies.", "Open the console"}),
	},
	ResourceOrganization: {
		AccessGranted: pair(
			alertCopy{"Vous avez accès à l'organisation « {{.Name}} »", "Nouvel accès",
				"Vous avez maintenant accès à l'organisation « {{.Name}} » sur betorun{{.By}}, et à ses projets selon votre rôle. Rôle : {{.Roles}}.", "Ouvrir la console"},
			alertCopy{"You now have access to organization \"{{.Name}}\"", "New access",
				"You now have access to organization \"{{.Name}}\" on betorun{{.By}}, and to its projects according to your role. Role: {{.Roles}}.", "Open the console"}),
		AccessChanged: pair(
			alertCopy{"Votre rôle dans l'organisation « {{.Name}} » a changé", "Rôle modifié",
				"Votre rôle dans l'organisation « {{.Name}} » a été modifié{{.By}}. Rôle : {{.Roles}}.", "Ouvrir la console"},
			alertCopy{"Your role in organization \"{{.Name}}\" changed", "Role changed",
				"Your role in organization \"{{.Name}}\" was changed{{.By}}. Role: {{.Roles}}.", "Open the console"}),
		AccessRemoved: pair(
			alertCopy{"Votre accès à l'organisation « {{.Name}} » a été retiré", "Accès retiré",
				"Vous n'avez plus accès à l'organisation « {{.Name}} »{{.By}}. Les accès accordés directement sur un projet restent valables.", "Ouvrir la console"},
			alertCopy{"Your access to organization \"{{.Name}}\" was removed", "Access removed",
				"You no longer have access to organization \"{{.Name}}\"{{.By}}. Access granted directly on a project still applies.", "Open the console"}),
	},
}

var groupCopies = map[bool]map[i18n.Locale]alertCopy{
	true: pair(
		alertCopy{"Vous faites partie du groupe « {{.Name}} »", "Ajouté à un groupe",
			"Vous avez été ajouté au groupe « {{.Name}} » de l'organisation « {{.Project}} »{{.By}}. Vous recevez les accès de ce groupe.", "Ouvrir la console"},
		alertCopy{"You are now in group \"{{.Name}}\"", "Added to a group",
			"You were added to group \"{{.Name}}\" in organization \"{{.Project}}\"{{.By}}. You receive this group's access.", "Open the console"}),
	false: pair(
		alertCopy{"Vous ne faites plus partie du groupe « {{.Name}} »", "Retiré d'un groupe",
			"Vous avez été retiré du groupe « {{.Name}} » de l'organisation « {{.Project}} ». Les accès de ce groupe ne s'appliquent plus à vous.", "Ouvrir la console"},
		alertCopy{"You are no longer in group \"{{.Name}}\"", "Removed from a group",
			"You were removed from group \"{{.Name}}\" in organization \"{{.Project}}\". This group's access no longer applies to you.", "Open the console"}),
}

func byText(lang i18n.Locale, by string) string {
	if strings.TrimSpace(by) == "" {
		return ""
	}
	if lang == i18n.EN {
		return " by " + plainValue(by)
	}
	return " par " + plainValue(by)
}

// rolesText : titres des rôles (en français) dans la langue de l'e-mail.
func rolesText(lang i18n.Locale, roles []string) string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, plainValue(i18n.Translate(lang, r)))
	}
	if len(out) == 0 {
		return "—"
	}
	return strings.Join(out, ", ")
}

// sendDirect : un e-mail à une personne, sans limite de fréquence.
func (n *Notifier) sendDirect(ctx context.Context, kind string, to domain.Recipient, copies map[i18n.Locale]alertCopy, name, context2, page string) {
	if n.Mail == nil || to.Email == "" {
		return
	}
	link := strings.TrimRight(n.ConsoleURL, "/") + page
	msg, err := renderWith(ctx, copies, to, context2, name, link, membershipOutro)
	if err == nil {
		err = n.Mail.Send(ctx, msg)
	}
	if err != nil {
		n.log().Warnw("membership email failed", "kind", kind, "error", err)
	}
}

// AccessChanged : rôles de `to` sur la ressource `name` (projet ou
// organisation) modifiés par `by` ; roles : titres des rôles après le
// changement (vide si retiré).
func (n *Notifier) AccessChanged(ctx context.Context, to domain.Recipient, resource, change, name string, roles []string, by string) {
	if n == nil {
		return
	}
	base, ok := accessCopies[resource][change]
	if !ok {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, len(base))
	for lang, c := range base {
		c.Body = strings.NewReplacer("{{.Roles}}", rolesText(lang, roles), "{{.By}}", byText(lang, by)).Replace(c.Body)
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		n.sendDirect(ctx, "access."+change, to, copies, name, name, "/dashboard")
	})
}

// GroupMembershipChanged : `to` ajouté au groupe `group` (ou retiré) de
// l'organisation `org`.
func (n *Notifier) GroupMembershipChanged(ctx context.Context, to domain.Recipient, group, org string, added bool, by string) {
	if n == nil {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, 2)
	for lang, c := range groupCopies[added] {
		c.Body = strings.ReplaceAll(c.Body, "{{.By}}", byText(lang, by))
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		n.sendDirect(ctx, "group.member", to, copies, group, org, "/groups")
	})
}
