package router

import (
	"net/http"

	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	mailmod "github.com/thekrauss/kubemanager/internal/modules/mail"
	mailSvc "github.com/thekrauss/kubemanager/internal/modules/mail/service"
)

// E-mails (docs/modules/mail.md) : les modules envoient par mail.Sender ; le
// message part dans la file asynq (Redis) et le worker le livre en SMTP, avec
// reprises. Sans Redis : envoi SMTP direct (comportement d'origine).

// mailSender : nil si l'envoi est désactivé (jamais une interface autour d'un nil).
func (a *App) mailSender() (mail.Sender, error) {
	smtp, err := a.smtpSender()
	if err != nil || smtp == nil {
		return nil, err
	}
	if a.Redis == nil {
		return smtp, nil
	}
	if a.mailQueue == nil {
		a.mailQueue = queue.NewRedisTaskDistributor(a.Redis, a.Logger.With("component", "mail-queue"))
	}
	return &mailSvc.QueueSender{Queue: a.mailQueue, Suppressions: a.mailSuppressions(),
		Logger: a.Logger.With("component", "mail")}, nil
}

func (a *App) mailSuppressions() mailSvc.Suppressions {
	if a.DB == nil {
		return nil
	}
	return &mailmod.GormRepository{DB: a.DB}
}

// startMailWorker : livraison des e-mails en file (worker). Rend la fonction
// d'arrêt (attend les envois en cours).
func (a *App) startMailWorker() (stop func(), err error) {
	smtp, err := a.smtpSender()
	if err != nil || smtp == nil || a.Redis == nil {
		if smtp == nil && err == nil {
			a.Logger.Warnw("mail worker disabled: mail is not enabled")
		}
		return func() {}, err
	}
	logger := a.Logger.With("component", "mail-worker")
	p := queue.NewRedisTaskProcessor(a.Redis, queue.ProcessorConfig{Concurrency: a.Config.Mail.QueueConcurrency}, logger)
	d := &mailSvc.Deliverer{SMTP: smtp, Suppressions: a.mailSuppressions(), Logger: logger}
	p.RegisterHandlers(queue.Handlers{HandleSendMail: d.HandleSendMail})
	if err := p.Start(); err != nil {
		return func() {}, err
	}
	return p.Shutdown, nil
}

func (a *App) newMailController() *mailmod.Controller {
	sup := a.mailSuppressions()
	if sup == nil {
		return nil
	}
	c := &mailmod.Controller{Admin: &mailSvc.Admin{Suppressions: sup}}
	if k := a.Config.Mail.SendGridWebhookPublicKey; k != "" {
		key, err := mailSvc.ParseSendGridPublicKey(k)
		if err != nil {
			a.Logger.Errorw("SendGrid webhook disabled: public key unreadable", "error", err)
		} else {
			c.SendGrid = &mailSvc.SendGridWebhook{Key: key, Suppressions: sup}
		}
	}
	return c
}

// addMailRoutes : webhook SendGrid (public, signé) et adresses mises de côté (équipe).
func addMailRoutes(app *App) {
	r := app.Controllers.Mail
	if r == nil {
		return
	}
	g := newGuards(app)
	hooks := RootGroup.NewGroup("/mail/webhooks", "Webhooks d'e-mail").Internal()
	hooks.AddRoute("/sendgrid", http.MethodPost, "Événements de SendGrid : rebonds et plaintes (signature ECDSA)", Public(), r.SendGridEvents)

	admin := RootGroup.NewGroup("/admin/mail", "Admin — e-mails").Internal()
	manage := g.Perm(iam.PlatformMailSuppressionsManage, security.PlatformResource)
	admin.AddRoute("/suppressions", http.MethodGet, "Adresses mises de côté (rebond, refus, plainte), les plus récentes d'abord", manage, tonic.Handler(r.ListSuppressions, http.StatusOK))
	admin.AddRoute("/suppressions", http.MethodDelete, "Relâcher une adresse : elle reçoit de nouveau les e-mails", manage, tonic.Handler(r.RemoveSuppression, http.StatusNoContent))
}
