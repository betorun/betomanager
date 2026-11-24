package service

import (
	"bytes"
	"context"
	"fmt"
	htmltemplate "html/template"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/background"
	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

const sendTimeout = 30 * time.Second

// MailNotifier : e-mails du support au client (accusé de réception, réponse
// de l'équipe, demande résolue) et à l'équipe (ticket prioritaire, message
// d'un client, assignation). Envoi en arrière-plan, erreurs journalisées.
type MailNotifier struct {
	Store      Store
	Mail       mail.Sender
	ConsoleURL string
	// StaffURL : console de l'équipe ; vide : aucun e-mail à l'équipe.
	StaffURL string
	// Staff : liste de l'équipe (tickets prioritaires) ; nil : pas d'envoi à tous.
	Staff   StaffStore
	Logger  *zap.SugaredLogger
	Spawner background.Spawner
	// Sync : envoi dans l'appel (tests).
	Sync bool
}

type supportCopy struct{ Subject, Title, Body, Button string }

// Textes : %s = sujet du ticket. Jamais de nom de fournisseur.
var (
	receivedCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Demande reçue : « %s »", "Nous avons bien reçu votre demande",
			"Votre demande « %s » est enregistrée. L'équipe betorun vous répond dans la console ; vous serez prévenu par e-mail.", "Voir la demande"},
		i18n.EN: {"Request received: \"%s\"", "We received your request",
			"Your request \"%s\" is recorded. The betorun team answers in the console; you will be notified by e-mail.", "View the request"},
	}
	replyCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Réponse à votre demande : « %s »", "L'équipe betorun vous a répondu",
			"Nouvelle réponse à votre demande « %s » :", "Répondre"},
		i18n.EN: {"Reply to your request: \"%s\"", "The betorun team replied",
			"New reply to your request \"%s\":", "Reply"},
	}
	resolvedCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Demande traitée : « %s »", "Votre demande a été traitée",
			"L'équipe betorun a clôturé votre demande « %s ». Si le problème persiste, rouvrez-la depuis la console.", "Voir la demande"},
		i18n.EN: {"Request handled: \"%s\"", "Your request was handled",
			"The betorun team closed your request \"%s\". If the problem persists, reopen it from the console.", "View the request"},
	}
	staffUrgentCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Ticket prioritaire : « %s »", "Nouveau ticket prioritaire",
			"Un client a ouvert le ticket « %s » en priorité haute ou urgente :", "Ouvrir le ticket"},
		i18n.EN: {"Priority ticket: \"%s\"", "New priority ticket",
			"A customer opened the ticket \"%s\" with high or urgent priority:", "Open the ticket"},
	}
	staffMessageCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Le client a répondu : « %s »", "Nouveau message d'un client",
			"Le client a répondu sur le ticket « %s », qui vous est assigné :", "Répondre"},
		i18n.EN: {"The customer replied: \"%s\"", "New message from a customer",
			"The customer replied on the ticket \"%s\", assigned to you:", "Reply"},
	}
	staffAssignedCopy = map[i18n.Locale]supportCopy{
		i18n.FR: {"Ticket assigné : « %s »", "Un ticket vous est assigné",
			"Le ticket « %s » vous a été assigné.", "Ouvrir le ticket"},
		i18n.EN: {"Ticket assigned: \"%s\"", "A ticket was assigned to you",
			"The ticket \"%s\" was assigned to you.", "Open the ticket"},
	}
	supportGreeting = map[i18n.Locale]string{i18n.FR: "Bonjour %s,", i18n.EN: "Hello %s,"}
)

func (n *MailNotifier) Received(ctx context.Context, t *domain.Ticket) {
	if t.CreatedByUserID == nil {
		return
	}
	n.send(ctx, t, receivedCopy, "", []uuid.UUID{*t.CreatedByUserID})
}

func (n *MailNotifier) AgentReplied(ctx context.Context, t *domain.Ticket, m *domain.Message, to []uuid.UUID) {
	n.send(ctx, t, replyCopy, m.Body, to)
}

func (n *MailNotifier) Resolved(ctx context.Context, t *domain.Ticket) {
	n.send(ctx, t, resolvedCopy, "", customerRecipients(t, nil))
}

func (n *MailNotifier) StaffUrgent(ctx context.Context, t *domain.Ticket) {
	if n == nil || n.Staff == nil || n.StaffURL == "" {
		return
	}
	n.staffSend(ctx, t, staffUrgentCopy, t.Description, func(ctx context.Context) ([]uuid.UUID, error) {
		agents, err := n.Staff.Agents(ctx)
		ids := make([]uuid.UUID, 0, len(agents))
		for _, a := range agents {
			ids = append(ids, a.UserID)
		}
		return ids, err
	})
}

func (n *MailNotifier) StaffCustomerMessage(ctx context.Context, t *domain.Ticket, m *domain.Message, agent uuid.UUID) {
	n.staffSend(ctx, t, staffMessageCopy, m.Body, fixed(agent))
}

func (n *MailNotifier) StaffAssigned(ctx context.Context, t *domain.Ticket, agent uuid.UUID) {
	n.staffSend(ctx, t, staffAssignedCopy, "", fixed(agent))
}

func fixed(ids ...uuid.UUID) func(context.Context) ([]uuid.UUID, error) {
	return func(context.Context) ([]uuid.UUID, error) { return ids, nil }
}

// staffSend : à l'équipe, lien vers le back-office ; pas de contrôle
// d'appartenance à l'organisation du ticket.
func (n *MailNotifier) staffSend(ctx context.Context, t *domain.Ticket, copies map[i18n.Locale]supportCopy, quote string,
	to func(context.Context) ([]uuid.UUID, error)) {
	if n == nil || n.StaffURL == "" {
		return
	}
	link := strings.TrimRight(n.StaffURL, "/") + "/admin/support?ticket=" + t.ID.String() + "&org=" + t.OrganizationID.String()
	n.dispatch(ctx, t, copies, quote, link, func(ctx context.Context) ([]Contact, error) {
		ids, err := to(ctx)
		if err != nil || len(ids) == 0 {
			return nil, err
		}
		return n.Store.Contacts(ctx, ids)
	})
}

func (n *MailNotifier) send(ctx context.Context, t *domain.Ticket, copies map[i18n.Locale]supportCopy, quote string, to []uuid.UUID) {
	if n == nil || len(to) == 0 {
		return
	}
	link := strings.TrimRight(n.ConsoleURL, "/") + "/support/" + t.ID.String()
	org := t.OrganizationID
	n.dispatch(ctx, t, copies, quote, link, func(ctx context.Context) ([]Contact, error) {
		return n.Store.MemberContacts(ctx, org, to)
	})
}

// dispatch : destinataires résolus puis envois, en arrière-plan.
func (n *MailNotifier) dispatch(ctx context.Context, t *domain.Ticket, copies map[i18n.Locale]supportCopy, quote, link string,
	recipients func(context.Context) ([]Contact, error)) {
	if n.Mail == nil || n.Store == nil {
		return
	}
	ticket := *t
	run := func(ctx context.Context) {
		contacts, err := recipients(ctx)
		if err != nil {
			n.log().Warnw("support e-mail recipients lookup failed", "ticket", ticket.ID, zap.Error(err))
			return
		}
		for _, c := range contacts {
			if err := n.Mail.Send(ctx, n.render(ctx, copies, c, &ticket, quote, link)); err != nil {
				n.log().Warnw("support e-mail failed", "ticket", ticket.ID, zap.Error(err))
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	if n.Sync {
		defer cancel()
		run(ctx)
		return
	}
	background.Go(n.Spawner, "support-mail", func(context.Context) {
		defer cancel()
		run(ctx)
	})
}

func (n *MailNotifier) log() *zap.SugaredLogger {
	if n.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return n.Logger
}

// render : langue choisie par la personne, sinon celle de la requête.
func (n *MailNotifier) render(ctx context.Context, copies map[i18n.Locale]supportCopy, to Contact, t *domain.Ticket, quote, link string) mail.Message {
	lang := i18n.From(ctx)
	switch to.Locale {
	case "en":
		lang = i18n.EN
	case "fr":
		lang = i18n.FR
	}
	if lang != i18n.EN {
		lang = i18n.FR
	}
	c := copies[lang]
	who := strings.TrimSpace(to.FullName)
	if who == "" {
		who, _, _ = strings.Cut(to.Email, "@")
	}
	data := map[string]string{"Lang": string(lang), "Title": c.Title, "Greeting": fmt.Sprintf(supportGreeting[lang], who),
		"Body": fmt.Sprintf(c.Body, t.Subject), "Quote": quote, "Button": c.Button, "Link": link}
	var text strings.Builder
	text.WriteString(c.Title + "\n\n" + data["Greeting"] + "\n\n" + data["Body"] + "\n")
	if quote != "" {
		text.WriteString("\n" + quote + "\n")
	}
	text.WriteString("\n" + c.Button + " : " + link + "\n\n— betorun\n")
	var html bytes.Buffer
	if err := supportHTML.Execute(&html, data); err != nil {
		n.log().Warnw("support e-mail template failed", zap.Error(err))
	}
	return mail.Message{To: to.Email, Subject: fmt.Sprintf(c.Subject, t.Subject), Text: text.String(), HTML: html.String()}
}

var supportHTML = htmltemplate.Must(htmltemplate.New("support").Parse(`<!doctype html>
<html lang="{{.Lang}}"><body style="margin:0;padding:24px;background:#f6f4f2;font-family:'IBM Plex Sans',Arial,sans-serif;color:#1d1a19">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:8px;padding:32px">
<tr><td style="font-size:20px;font-weight:600;color:#7A1F3D;padding-bottom:16px">betorun</td></tr>
<tr><td style="font-size:18px;font-weight:600;padding-bottom:12px">{{.Title}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;padding-bottom:8px">{{.Greeting}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;padding-bottom:16px">{{.Body}}</td></tr>
{{- if .Quote}}
<tr><td style="font-size:15px;line-height:1.5;padding:12px 16px;margin-bottom:16px;border-left:3px solid #7A1F3D;background:#f6f4f2;white-space:pre-wrap">{{.Quote}}</td></tr>
<tr><td style="height:16px"></td></tr>
{{- end}}
<tr><td style="padding-bottom:8px"><a href="{{.Link}}" style="display:inline-block;background:#7A1F3D;color:#ffffff;text-decoration:none;padding:12px 20px;border-radius:6px;font-weight:600">{{.Button}}</a></td></tr>
</table></td></tr></table></body></html>`))
