package alerts

import (
	"context"
	"strings"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// « Me prévenir » (ADR 0013) : un service annoncé « Bientôt » vient d'ouvrir.
// Un seul e-mail par demande ; le texte ne reprend que le nom du service.

const pageServices = "/services"

var addonAvailableCopies = pair(
	alertCopy{
		Subject: "{{.Name}} est disponible sur betorun",
		Title:   "Un service que vous attendiez est disponible",
		Body:    "Vous avez demandé à être prévenu : {{.Name}} est maintenant au catalogue. Vous pouvez l'ajouter à vos projets depuis la page Services, et le brancher sur vos conteneurs en un clic.",
		Button:  "Ouvrir les services",
	},
	alertCopy{
		Subject: "{{.Name}} is available on betorun",
		Title:   "A service you were waiting for is available",
		Body:    "You asked to be notified: {{.Name}} is now in the catalog. You can add it to your projects from the Services page and attach it to your containers in one click.",
		Button:  "Open services",
	},
)

var waitlistOutro = map[i18n.Locale]string{
	i18n.FR: "Vous recevez cet email parce que vous avez cliqué sur « Me prévenir ». C'est le seul que nous vous enverrons à ce sujet.",
	i18n.EN: "You receive this email because you clicked “Notify me”. It is the only one we will send you about it.",
}

// AddonAvailable : prévient une personne qu'un service annoncé est ouvert.
func (n *Notifier) AddonAvailable(ctx context.Context, lang i18n.Locale, email, fullName, title string) error {
	if n == nil || n.Mail == nil {
		return nil
	}
	ctx = i18n.With(ctx, lang)
	link := strings.TrimRight(n.ConsoleURL, "/") + pageServices
	msg, err := renderWith(ctx, addonAvailableCopies, Recipient{Email: email, FullName: fullName}, "", title, link, waitlistOutro)
	if err != nil {
		return err
	}
	return n.Mail.Send(ctx, msg)
}
