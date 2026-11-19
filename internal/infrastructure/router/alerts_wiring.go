package router

import (
	"context"

	"github.com/thekrauss/kubemanager/internal/iam"
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

// wireAPIKeyAlerts : e-mail aux propriétaires et éditeurs du projet à chaque
// nouvelle clé API d'un compte de service.
func (a *App) wireAPIKeyAlerts(sa *iam.ServiceAccountService) {
	n := a.alertNotifier()
	if n == nil {
		return
	}
	sa.OnKeyCreated = func(ctx context.Context, acct *iam.ServiceAccount, key iam.APIKey, caller iam.Principal) {
		account := acct.DisplayName
		if account == "" {
			account = acct.AccountID
		}
		n.APIKeyCreated(ctx, acct.ProjectID, key.ID, account, key.DisplayName, a.principalLabel(ctx, caller), key.ExpiresAt)
	}
}

// principalLabel : adresse e-mail d'une personne, sinon le principal tel quel.
func (a *App) principalLabel(ctx context.Context, p iam.Principal) string {
	kind, id, err := p.Parse()
	if err != nil || kind != iam.KindUser {
		return string(p)
	}
	var email string
	if err := a.DB.WithContext(ctx).Table("users").Select("email").Where("id = ?", id).Scan(&email).Error; err != nil || email == "" {
		return string(p)
	}
	return email
}
