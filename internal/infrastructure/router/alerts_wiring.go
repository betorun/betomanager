package router

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"net/http"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/alerts"
)

// smtpSender : serveur SMTP de la configuration ; nil si l'envoi est désactivé.
// Seul le worker l'appelle directement : les modules passent par mailSender.
func (a *App) smtpSender() (*mail.SMTP, error) {
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
	n := &alerts.Notifier{Store: alerts.NewStore(a.DB), Mail: sender, ConsoleURL: a.Config.Frontend.BaseURL,
		Logger: a.Logger.With("component", "alerts"), UnsubscribeKey: a.unsubscribeKey(), APIURL: a.Config.Builds.PublicAPIURL,
		Spawner: a.spawner()}
	if w := a.alertWebhooks(); w != nil { // pas d'interface non nulle autour d'un nil
		n.Webhooks = w
	}
	return n
}

// alertWebhooks : webhooks d'alertes des projets ; nil sans clé de chiffrement.
func (a *App) alertWebhooks() *alerts.WebhookService {
	sealer, err := auth.NewSealer(a.Config.IAM.STSKeyEncryptionKey)
	if err != nil {
		return nil
	}
	return &alerts.WebhookService{DB: a.DB, Sealer: sealer, Client: alerts.PublicHTTPClient()}
}

// unsubscribeKey : clé des liens de désabonnement, dérivée du poivre des clés
// API (déjà secret et propre à l'installation) ; nil sans poivre.
func (a *App) unsubscribeKey() []byte {
	pepper := a.Config.IAM.APIKeyPepper
	if pepper == "" {
		return nil
	}
	m := hmac.New(sha256.New, []byte(pepper))
	m.Write([]byte("betorun alerts unsubscribe v1"))
	return m.Sum(nil)
}

func (a *App) newAlertsController() *alerts.Controller {
	c := &alerts.Controller{Prefs: &alerts.PreferenceStore{DB: a.DB}, Key: a.unsubscribeKey(),
		Webhooks: a.alertWebhooks(), ProjectName: alerts.NewStore(a.DB).ProjectName, ConsoleURL: a.Config.Frontend.BaseURL}
	return c
}

func addAlertPreferenceRoutes(app *App) {
	r := app.Controllers.Alerts
	if r == nil {
		return
	}
	IdentityGroup.AddRoute("/me/preferences", http.MethodGet, "Préférences d'e-mail : langue et alertes de projet reçues",
		Authenticated(), tonic.Handler(r.GetPreferences, http.StatusOK))
	IdentityGroup.AddRoute("/me/preferences", http.MethodPatch, "Changer la langue des e-mails ou couper une catégorie d'alertes (sécurité, accès et facturation ne se coupent pas)",
		Authenticated(), tonic.Handler(r.UpdatePreferences, http.StatusOK))
	g := RootGroup.NewGroup("/alerts", "Alertes").
		Named("Alertes", "Désabonnement des alertes e-mail par le lien reçu.")
	g.AddRoute("/unsubscribe", http.MethodPost, "Ne plus recevoir une catégorie d'alertes (jeton du lien reçu ; désabonnement en un clic)",
		Public(), r.Unsubscribe)

	guards := newGuards(app)
	project := security.ProjectParam("id")
	w := RootGroup.NewGroup("/projects/:id/alert-webhooks", "Webhooks d'alertes").
		Named("Webhooks d'alertes", "Alertes du projet envoyées aussi vers Slack ou vers votre service (JSON signé).")
	w.AddRoute("", http.MethodGet, "Webhooks d'alertes du projet (adresse masquée, dernier envoi)",
		guards.Perm(iam.ProjectsGet, project), tonic.Handler(r.ListWebhooks, http.StatusOK))
	w.AddRoute("", http.MethodPost, "Ajouter un webhook d'alertes (Slack ou JSON signé ; secret de signature montré une fois)",
		guards.Perm(iam.ProjectsUpdate, project), tonic.Handler(r.CreateWebhook, http.StatusCreated))
	w.AddRoute("/:webhookID/test", http.MethodPost, "Envoyer un message d'essai au webhook",
		guards.Perm(iam.ProjectsUpdate, project), tonic.Handler(r.TestWebhook, http.StatusOK))
	w.AddRoute("/:webhookID", http.MethodDelete, "Supprimer un webhook d'alertes",
		guards.Perm(iam.ProjectsUpdate, project), tonic.Handler(r.DeleteWebhook, http.StatusNoContent))
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
