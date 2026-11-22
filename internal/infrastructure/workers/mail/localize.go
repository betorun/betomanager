package mail

import (
	"context"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// localizeMail met un mail dans la langue de son destinataire (sa dernière
// langue d'écran, retrouvée par son adresse) : l'objet et les variables connues
// du catalogue sont traduits, le gabarit pris dans sa version traduite s'il en
// a une. Un destinataire sans compte (prospect, candidat) reçoit le français.
func localizeMail(ctx context.Context, to, subject, template string, vars map[string]string) (string, string, map[string]string) {
	l := prefs.ForEmail(ctx, to)
	localized := make(map[string]string, len(vars))
	for k, v := range vars {
		localized[k] = i18n.Translate(l, v)
	}
	return i18n.Translate(l, subject), mailSvc.LocalizedTemplate(template, string(l)), localized
}
