package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Accès d'assistance (ADR 0017) : les propriétaires de l'organisation sont
// prévenus d'une demande de l'équipe, d'un accès accordé (par l'un d'eux) et,
// tout de suite, d'un bris de glace. Jamais coupé : c'est un accès à leurs
// ressources. {{.Project}} : organisation ; {{.Name}} : la personne de
// l'équipe ; {{.Reason}} et {{.Until}} remplacés avant le rendu.

const pageAssistance = "/team-access"

var assistanceOutro = map[i18n.Locale]string{
	i18n.FR: "Vous recevez cet e-mail parce que vous êtes propriétaire de cette organisation. L'équipe betorun n'a aucun accès à vos ressources sans un accès d'assistance.",
	i18n.EN: "You receive this email because you own this organization. The betorun team has no access to your resources without an assistance access.",
}

var assistanceCopies = map[string]map[i18n.Locale]alertCopy{
	"requested": pair(
		alertCopy{"L'équipe betorun demande un accès à « {{.Project}} »", "Demande d'accès d'assistance",
			"{{.Name}}, de l'équipe betorun, demande un accès temporaire à l'organisation « {{.Project}} » pour traiter votre demande. Motif : {{.Reason}}. Rien n'est ouvert tant qu'un propriétaire n'a pas accepté.", "Voir la demande"},
		alertCopy{"The betorun team requests access to \"{{.Project}}\"", "Assistance access request",
			"{{.Name}}, from the betorun team, requests temporary access to organization \"{{.Project}}\" to handle your request. Reason: {{.Reason}}. Nothing is opened until an owner accepts.", "View the request"}),
	"granted": pair(
		alertCopy{"Accès d'assistance ouvert sur « {{.Project}} »", "Accès d'assistance accordé",
			"{{.Name}}, de l'équipe betorun, a accès à l'organisation « {{.Project}} » jusqu'au {{.Until}} (UTC) : lecture et actions réversibles. Motif : {{.Reason}}. Vous pouvez le révoquer à tout moment.", "Gérer les accès"},
		alertCopy{"Assistance access opened on \"{{.Project}}\"", "Assistance access granted",
			"{{.Name}}, from the betorun team, has access to organization \"{{.Project}}\" until {{.Until}} (UTC): read and reversible actions. Reason: {{.Reason}}. You can revoke it at any time.", "Manage access"}),
	"breakglass": pair(
		alertCopy{"Accès d'urgence de l'équipe betorun sur « {{.Project}} »", "Accès d'urgence ouvert",
			"{{.Name}}, de l'équipe betorun, a ouvert un accès d'urgence à l'organisation « {{.Project}} » jusqu'au {{.Until}} (UTC), sans votre accord préalable. Motif : {{.Reason}}. Chaque action est inscrite dans votre journal d'audit ; vous pouvez révoquer l'accès à tout moment.", "Gérer les accès"},
		alertCopy{"Emergency access by the betorun team on \"{{.Project}}\"", "Emergency access opened",
			"{{.Name}}, from the betorun team, opened emergency access to organization \"{{.Project}}\" until {{.Until}} (UTC), without your prior consent. Reason: {{.Reason}}. Every action is recorded in your audit log; you can revoke the access at any time.", "Manage access"}),
}

// AssistanceNotice : e-mail aux propriétaires de l'organisation.
func (n *Notifier) AssistanceNotice(ctx context.Context, org uuid.UUID, kind, staff, reason string, until *time.Time) {
	if n == nil {
		return
	}
	base, ok := assistanceCopies[kind]
	if !ok {
		return
	}
	date := ""
	if until != nil {
		date = until.UTC().Format("2006-01-02 15:04")
	}
	copies := make(map[i18n.Locale]alertCopy, len(base))
	for lang, c := range base {
		c.Body = strings.NewReplacer("{{.Reason}}", plainValue(reason), "{{.Until}}", date).Replace(c.Body)
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		if n.Mail == nil || n.Store == nil {
			return
		}
		recipients, err := n.Store.OrgRecipients(ctx, org)
		if err != nil {
			n.log().Warnw("assistance notice recipients lookup failed", "organization", org, zap.Error(err))
			return
		}
		name, err := n.Store.OrgName(ctx, org)
		if err != nil {
			return
		}
		link := strings.TrimRight(n.ConsoleURL, "/") + pageAssistance
		for _, r := range recipients {
			msg, err := renderWith(ctx, copies, r, name, plainValue(staff), link, assistanceOutro)
			if err == nil {
				err = n.Mail.Send(ctx, msg)
			}
			if err != nil {
				n.log().Warnw("assistance notice failed", "kind", kind, "error", err)
			}
		}
	})
}
