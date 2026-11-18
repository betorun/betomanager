package router

import (
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/modules/alerts"
)

// mailSender : serveur SMTP de la configuration ; nil si l'envoi est désactivé.
func (a *App) mailSender() (*mail.SMTP, error) {
	m := a.Config.Mail
	if !m.Enabled {
		return nil, nil
	}
	return mail.NewSMTP(mail.Config{Host: m.SMTPHost, Port: m.SMTPPort, User: m.SMTPUser, Password: m.SMTPPassword,
		FromEmail: m.FromEmail, FromName: m.FromName})
}

// alertNotifier : alertes email (incidents de conteneurs, builds échoués) ;
// nil sans envoi d'emails ou sans URL de la console pour les liens.
func (a *App) alertNotifier() *alerts.Notifier {
	sender, err := a.mailSender()
	if err != nil {
		a.Logger.Warnw("alerts disabled: mail configuration invalid", "error", err)
		return nil
	}
	if sender == nil || a.Config.Frontend.BaseURL == "" {
		a.Logger.Warnw("alerts disabled: mail or console URL not configured")
		return nil
	}
	return &alerts.Notifier{Store: alerts.NewStore(a.DB), Mail: sender, ConsoleURL: a.Config.Frontend.BaseURL,
		Logger: a.Logger.With("component", "alerts")}
}
