package router

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail/imapbox"
	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	computeRepo "github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository/ovhvps"
	computeSvc "github.com/thekrauss/kubemanager/internal/modules/compute/service"
	computeWorkflows "github.com/thekrauss/kubemanager/internal/modules/compute/workflows"
)

// VPS revendus (docs/modules/compute.md) : l'API écrit des demandes (outbox),
// le relais du worker lance les workflows Temporal, le mode compute les
// exécute avec les clés du fournisseur.

func (a *App) computeTaskQueue() string {
	if q := a.Config.Compute.TaskQueue; q != "" {
		return q
	}
	return computeWorkflows.DefaultTaskQueue
}

func (a *App) computeService() *computeSvc.Service {
	if a.DB == nil || !a.Config.Compute.Enabled {
		return nil
	}
	s := &computeSvc.Service{Store: &computeRepo.GormStore{DB: a.DB}, Ordering: a.Config.Compute.OrderingEnabled}
	if e := a.computeEvents(); e != nil {
		s.Events = e
	}
	if sealer, err := auth.NewSealer(a.Config.IAM.STSKeyEncryptionKey); err == nil {
		s.Sealer = sealer
	}
	if legal := a.legalService(); legal != nil { // pas d'interface non nulle autour d'un nil
		s.Terms = legal
	}
	if billing := a.billingService(); billing != nil {
		s.Catalogs = computeCatalogs{billing: billing}
		s.Wallet = billing
	}
	return s
}

// startComputeRenewals : débit mensuel des VPS (worker) ; non payé : suspension.
func (a *App) startComputeRenewals(ctx context.Context) {
	s := a.computeService()
	if s == nil || s.Wallet == nil || s.Catalogs == nil {
		return
	}
	log := a.Logger.With("component", "compute-renewals")
	a.goWith(ctx, "compute-renewals", func(ctx context.Context) {
		t := time.NewTicker(computeSvc.DefaultRenewEvery)
		defer t.Stop()
		for {
			renewed, suspended, err := s.RenewDue(ctx)
			switch {
			case err != nil:
				log.Errorw("VPS renewals failed", "error", err)
			case renewed+suspended > 0:
				log.Infow("VPS renewals", "renewed", renewed, "suspended", suspended)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// computeEvents : changements de VPS publiés sur Centrifugo (nil sans temps réel).
func (a *App) computeEvents() *vpsEvents {
	if a.Publisher == nil {
		return nil
	}
	return &vpsEvents{pub: a.Publisher, log: a.Logger}
}

type vpsEvents struct {
	pub realtime.Publisher
	log interface {
		Warnw(msg string, kv ...any)
	}
}

// VPSEvent : publié à chaque changement d'un VPS.
type VPSEvent struct {
	Type string             `json:"type"`
	VPS  *computeDomain.VPS `json:"vps"`
}

// BackupsChanged : la console relit GET …/backups.
func (e *vpsEvents) BackupsChanged(ctx context.Context, v *computeDomain.VPS) {
	ev := map[string]string{"type": "vps.backups.updated", "vps_id": v.ID.String()}
	if err := e.pub.Publish(ctx, realtime.VPSChannel(v.ProjectID.String(), v.ID.String()), ev); err != nil {
		e.log.Warnw("compute: backups event not published", "vps", v.ID, "error", err)
	}
}

func (e *vpsEvents) VPSChanged(ctx context.Context, v *computeDomain.VPS) {
	ev := VPSEvent{Type: "vps.updated", VPS: v}
	for _, ch := range []string{realtime.VPSChannel(v.ProjectID.String(), v.ID.String()), realtime.ProjectChannel(v.ProjectID.String())} {
		if err := e.pub.Publish(ctx, ch, ev); err != nil {
			e.log.Warnw("compute: VPS event not published", "vps", v.ID, "channel", ch, "error", err)
		}
	}
}

// computeSuspender : VPS arrêtés à la suspension d'une organisation (module
// billing) ; sans dépendre de billingService (pas de boucle).
func (a *App) computeSuspender() *computeSvc.Service {
	if a.DB == nil || !a.Config.Compute.Enabled {
		return nil
	}
	s := &computeSvc.Service{Store: &computeRepo.GormStore{DB: a.DB}}
	if e := a.computeEvents(); e != nil {
		s.Events = e
	}
	return s
}

// vpsProject : projet du VPS de l'URL, s'il est bien dans l'organisation de
// l'URL (sinon refus, sans dire si le VPS existe ailleurs).
func vpsProject(r *compute.Controller) security.ResourceResolver {
	return func(c *gin.Context) (iam.ResourceName, error) {
		org, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
		if err != nil {
			return "", security.ErrResourceRefInvalid
		}
		id, err := uuid.Parse(strings.TrimSpace(c.Param("vpsID")))
		if err != nil {
			return "", security.ErrResourceRefInvalid
		}
		if r.Service == nil {
			return iam.OrganizationName(org), nil
		}
		v, err := r.Service.Store.VPS(c.Request.Context(), id)
		if err != nil || v.OrganizationID != org {
			return iam.OrganizationName(org), nil // droit de l'organisation, puis 404 par le contrôleur
		}
		return iam.ProjectName(v.ProjectID), nil
	}
}

// computeCatalogs : taux de la grille de l'organisation (module billing).
type computeCatalogs struct {
	billing interface {
		AccountCatalogRate(ctx context.Context, org uuid.UUID) (rate, roundTo int64, currency string, err error)
	}
}

// Rates : taux euro des grilles en vigueur (aperçu des prix des offres).
func (c computeCatalogs) Rates(ctx context.Context) ([]computeDomain.Rate, error) {
	lister, ok := c.billing.(interface {
		Catalogs(ctx context.Context) (*billingDomain.CatalogsResponse, error)
	})
	if !ok {
		return nil, nil
	}
	res, err := lister.Catalogs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]computeDomain.Rate, 0, len(res.Catalogs))
	for _, cat := range res.Catalogs {
		out = append(out, computeDomain.Rate{Currency: cat.Currency, EURMilli: cat.EURRateMilli, RoundTo: cat.DomainRoundTo})
	}
	return out, nil
}

func (c computeCatalogs) Rate(ctx context.Context, org uuid.UUID) (computeDomain.Rate, error) {
	rate, round, currency, err := c.billing.AccountCatalogRate(ctx, org)
	return computeDomain.Rate{Currency: currency, EURMilli: rate, RoundTo: round}, err
}

// temporalStarter : WorkflowID = identifiant de la demande, jamais relancé.
type temporalStarter struct {
	dist  temporal.TaskDistributor
	queue string
}

func (s temporalStarter) Start(ctx context.Context, workflowID, _ string, input any) error {
	_, err := s.dist.ExecuteWorkflow(ctx, temporal.ExecuteOptions{WorkflowID: workflowID, TaskQueue: s.queue, RejectDuplicate: true},
		computeWorkflows.RequestWorkflowName, input)
	if temporal.IsAlreadyStarted(err) {
		return computeSvc.ErrAlreadyStarted
	}
	return err
}

// startComputeRelay : dans le worker (il a le client Temporal).
func (a *App) startComputeRelay(ctx context.Context) {
	if !a.Config.Compute.Enabled || a.DB == nil || a.TemporalDistributor == nil {
		return
	}
	r := &computeSvc.Relay{Store: &computeRepo.GormStore{DB: a.DB},
		Starter: temporalStarter{dist: a.TemporalDistributor, queue: a.computeTaskQueue()},
		Logger:  a.Logger.With("component", "compute-relay")}
	a.goWith(ctx, "compute-relay", func(ctx context.Context) { r.Run(ctx, computeSvc.DefaultRelayEvery) })
}

// RunCompute : APP_MODE=compute, worker Temporal de la file « compute ». Seul
// process qui charge les clés du fournisseur (lot 2 : OvhProvider).
func (a *App) RunCompute(ctx context.Context) error {
	defer a.shutdownInfra()
	if a.Temporal.Client == nil {
		return fmt.Errorf("compute: temporal client not initialized")
	}
	provider := a.computeProvider()
	w := worker.New(a.Temporal.Client, a.computeTaskQueue(), worker.Options{})
	w.RegisterWorkflowWithOptions(computeWorkflows.RequestWorkflow, workflow.RegisterOptions{Name: computeWorkflows.RequestWorkflowName})
	w.RegisterWorkflowWithOptions(computeWorkflows.ReconcileWorkflow, workflow.RegisterOptions{Name: computeWorkflows.ReconcileWorkflowName})
	activities := &computeWorkflows.Activities{Store: &computeRepo.GormStore{DB: a.DB}}
	if provider != nil { // interfaces nil si aucune clé
		activities.Provider = provider
		activities.Orderer = provider
		activities.Backups = provider
		activities.Rescue = provider
		activities.Network = provider
		activities.Stock = provider
	}
	if billing := a.billingService(); billing != nil {
		activities.Wallet = billing
	}
	if e := a.computeEvents(); e != nil {
		activities.Events = e
	}
	if sealer, err := auth.NewSealer(a.Config.IAM.STSKeyEncryptionKey); err == nil {
		activities.Sealer = sealer
	} else {
		a.Logger.Warnw("compute: no secrets key, rescue passwords cannot be kept", "error", err)
	}
	m := a.Config.Compute.IMAP
	if box, err := imapbox.New(imapbox.Config{Host: m.Host, Port: m.Port, Username: m.Username, Password: m.Password, Mailbox: m.Mailbox}); err == nil {
		mb := &ovhvps.Mailbox{Box: box}
		activities.Mailbox = mb
		activities.Inbox = mb
	} else {
		a.Logger.Warnw("compute: no technical mailbox, rescue passwords will be reported missing", "error", err)
	}
	w.RegisterActivity(activities)
	if err := w.Start(); err != nil {
		return fmt.Errorf("compute worker: %w", err)
	}
	defer w.Stop()
	a.Logger.Infow("compute worker started", "task_queue", a.computeTaskQueue(), "provider", provider != nil)
	if provider != nil {
		a.scheduleComputeReconcile(ctx)
	}
	healthSrv := a.startHealthServer()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	a.Logger.Infow("compute worker shutdown signal received")
	if healthSrv != nil {
		_ = healthSrv.Shutdown(context.Background())
	}
	return nil
}

// computeProvider : OVH si ses clés sont posées ; nil sinon (le mode compute
// tourne quand même : diagnostic, et plus tard d'autres fournisseurs).
func (a *App) computeProvider() *ovhvps.Provider {
	c := a.Config.Compute.OVH
	p, err := ovhvps.New(ovh.Credentials{Endpoint: c.Endpoint, ApplicationKey: c.ApplicationKey,
		ApplicationSecret: c.ApplicationSecret, ConsumerKey: c.ConsumerKey}, c.Subsidiary)
	if err != nil {
		a.Logger.Warnw("compute: no provider credentials, reconciliation disabled", "error", err)
		return nil
	}
	return p
}

// scheduleComputeReconcile : workflow périodique unique (identifiant fixe) ;
// déjà planifié par un autre process compute : rien à faire. Pour changer
// la fréquence, arrêter le workflow compute-reconcile dans Temporal.
func (a *App) scheduleComputeReconcile(ctx context.Context) {
	_, err := a.Temporal.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       computeWorkflows.ReconcileWorkflowID,
		TaskQueue:                                a.computeTaskQueue(),
		CronSchedule:                             computeWorkflows.ReconcileCron,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, computeWorkflows.ReconcileWorkflowName)
	switch {
	case err == nil:
		a.Logger.Infow("compute reconciliation scheduled", "cron", computeWorkflows.ReconcileCron)
	case temporal.IsAlreadyStarted(err):
	default:
		a.Logger.Errorw("compute reconciliation not scheduled", "error", err)
	}
}

func (a *App) newComputeController() *compute.Controller {
	s := a.computeService()
	if s == nil || s.Catalogs == nil {
		return nil
	}
	return &compute.Controller{Service: s, DB: a.DB}
}

// addComputeRoutes : offres (clients) ; réglage et diagnostic (équipe, /admin).
func addComputeRoutes(app *App) {
	r := app.Controllers.Compute
	if r == nil {
		return
	}
	g := newGuards(app)
	org := RootGroup.NewGroup("/organizations/:id/compute", "Serveurs VPS").
		Named("Serveurs VPS", "Serveurs privés virtuels : offres et prix mensuels.")
	org.AddRoute("/offers", http.MethodGet, "Offres de VPS en vente, prix mensuel dans la monnaie de l'organisation",
		g.Perm(iam.BillingAccountsGet, security.OrganizationParam("id")), tonic.Handler(r.Offers, http.StatusOK))
	// Un VPS appartient à un projet : ses droits se vérifient sur ce projet
	// (un droit sur l'organisation est hérité).
	onVPS := func(p iam.Permission) Access { return g.Perm(p, vpsProject(r)) }
	org.AddRoute("/vps", http.MethodGet, "VPS de l'organisation", g.Perm(iam.VPSList, security.OrganizationParam("id")), tonic.Handler(r.List, http.StatusOK))
	org.AddRoute("/vps/:vpsID", http.MethodGet, "Un VPS de l'organisation", onVPS(iam.VPSGet), tonic.Handler(r.Get, http.StatusOK))
	org.AddRoute("/vps/:vpsID/start", http.MethodPost, "Démarrer un VPS", onVPS(iam.VPSStart), tonic.Handler(r.Start, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/stop", http.MethodPost, "Arrêter un VPS", onVPS(iam.VPSStop), tonic.Handler(r.Stop, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/reboot", http.MethodPost, "Redémarrer un VPS", onVPS(iam.VPSReboot), tonic.Handler(r.Reboot, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/reinstall", http.MethodPost, "Réinstaller un VPS avec un système et une clé SSH : son disque est effacé",
		onVPS(iam.VPSReinstall), tonic.Handler(r.Reinstall, http.StatusAccepted))
	// Mode secours : mot de passe intercepté, chiffré, affiché une seule fois.
	org.AddRoute("/vps/:vpsID/rescue", http.MethodPost, "Redémarrer un VPS sur le système de secours",
		onVPS(iam.VPSRescue), tonic.Handler(r.EnterRescue, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/rescue/exit", http.MethodPost, "Quitter le mode secours : le VPS redémarre sur son disque",
		onVPS(iam.VPSRescue), tonic.Handler(r.ExitRescue, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/rescue/password", http.MethodPost, "Afficher le mot de passe du mode secours (une seule fois)",
		onVPS(iam.VPSRescue), tonic.Handler(r.RevealRescue, http.StatusOK))
	// Réseau et fin de service.
	org.AddRoute("/vps/:vpsID/reverse", http.MethodPut, "Régler le DNS inverse d'une adresse du VPS (le nom doit déjà pointer vers l'adresse)",
		onVPS(iam.VPSNetwork), tonic.Handler(r.SetReverse, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/terminate", http.MethodPost, "Résilier un VPS : coupé tout de suite, plus renouvelé, le mois entamé n'est pas remboursé",
		onVPS(iam.VPSDelete), tonic.Handler(r.Terminate, http.StatusAccepted))
	// Sauvegardes et snapshot (l'option se paie : droit de paiement sur l'organisation).
	pay := g.Perm(iam.BillingPaymentsCreate, security.OrganizationParam("id"))
	org.AddRoute("/vps/:vpsID/backups", http.MethodGet, "Sauvegardes d'un VPS : points de restauration, sauvegardes montées, snapshot",
		onVPS(iam.VPSGet), tonic.Handler(r.Backups, http.StatusOK))
	org.AddRoute("/vps/:vpsID/backups/restore", http.MethodPost, "Restaurer un VPS depuis une sauvegarde (full : remplace le disque ; mount : disque supplémentaire)",
		onVPS(iam.VPSRestore), tonic.Handler(r.Restore, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/backups/unmount", http.MethodPost, "Détacher une sauvegarde montée",
		onVPS(iam.VPSRestore), tonic.Handler(r.Unmount, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/snapshot", http.MethodPost, "Créer le snapshot d'un VPS (un seul à la fois)",
		onVPS(iam.VPSSnapshot), tonic.Handler(r.CreateSnapshot, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/snapshot", http.MethodDelete, "Supprimer le snapshot d'un VPS",
		onVPS(iam.VPSSnapshot), tonic.Handler(r.DeleteSnapshot, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/snapshot/revert", http.MethodPost, "Revenir au snapshot : le disque reprend son état",
		onVPS(iam.VPSRestore), tonic.Handler(r.RevertSnapshot, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/snapshot/subscribe", http.MethodPost, "Souscrire l'option snapshot : un mois débité du solde, renouvelé avec le VPS",
		pay, tonic.Handler(r.SubscribeSnapshot, http.StatusAccepted))
	org.AddRoute("/vps/:vpsID/snapshot/unsubscribe", http.MethodPost, "Résilier l'option snapshot : le snapshot est supprimé, le mois entamé n'est pas remboursé",
		pay, tonic.Handler(r.UnsubscribeSnapshot, http.StatusAccepted))
	org.AddRoute("/vps", http.MethodPost, "Commander un VPS : le premier mois est débité du solde, puis le serveur est créé avec la clé SSH fournie",
		g.Perm(iam.BillingPaymentsCreate, security.OrganizationParam("id")), tonic.Handler(r.Order, http.StatusAccepted))

	admin := RootGroup.NewGroup("/admin/compute", "Admin — VPS").Internal()
	prices := g.Perm(iam.PlatformBillingCatalogsGet, security.PlatformResource)
	setPrices := g.Perm(iam.PlatformBillingCatalogsUpdate, security.PlatformResource)
	view := g.Perm(iam.PlatformComputeGet, security.PlatformResource)
	manage := g.Perm(iam.PlatformComputeManage, security.PlatformResource)
	admin.AddRoute("/offers", http.MethodGet, "Offres de VPS (prix fournisseur, coefficient, emplacements, systèmes)", prices, tonic.Handler(r.AdminOffers, http.StatusOK))
	admin.AddRoute("/offers/:code", http.MethodPut, "Régler une offre de VPS (prix fournisseur, coefficient, palier)", setPrices, tonic.Handler(r.SaveOffer, http.StatusOK))
	admin.AddRoute("/vps", http.MethodGet, "VPS de tous les clients, relevé du fournisseur, écarts et dernières réconciliations", view, tonic.Handler(r.Fleet, http.StatusOK))
	admin.AddRoute("/reconcile", http.MethodPost, "Lancer une réconciliation des VPS avec le fournisseur", manage, tonic.Handler(r.Reconcile, http.StatusAccepted))
	admin.AddRoute("/vps/:vpsID/spam/unblock", http.MethodPost, "Lever le blocage anti-spam de l'adresse d'un VPS (après vérification)", manage, tonic.Handler(r.UnblockSpam, http.StatusAccepted))
	admin.AddRoute("/vps/:vpsID/smtp", http.MethodPut, "Ouvrir ou fermer le port 25 sortant d'un VPS (prochaine installation)", manage, tonic.Handler(r.SetSMTP, http.StatusOK))
	// Lot 8 : quotas et anti-fraude, procédure abus, e-mails du fournisseur.
	admin.AddRoute("/limits", http.MethodGet, "Quotas des VPS et limites des comptes récents", view, tonic.Handler(r.Limits, http.StatusOK))
	admin.AddRoute("/limits", http.MethodPut, "Régler les quotas des VPS et les limites des comptes récents", manage, tonic.Handler(r.SaveLimits, http.StatusOK))
	admin.AddRoute("/organizations/:id/limits", http.MethodGet, "Quota de VPS d'une organisation", view, tonic.Handler(r.OrgLimits, http.StatusOK))
	admin.AddRoute("/organizations/:id/limits", http.MethodPut, "Accorder un quota de VPS à une organisation, ou la dispenser des limites des comptes récents",
		g.Perm(iam.PlatformComputeManage, security.PlatformResource).Staff(security.StaffOrganization("id")), tonic.Handler(r.SetOrgLimits, http.StatusOK))
	admin.AddRoute("/vps/:vpsID/abuse/suspend", http.MethodPost, "Suspendre un VPS pour abus (arrêt immédiat, motif montré au client)", manage, tonic.Handler(r.SuspendForAbuse, http.StatusAccepted))
	admin.AddRoute("/vps/:vpsID/abuse/resume", http.MethodPost, "Lever la suspension pour abus d'un VPS", manage, tonic.Handler(r.ResumeFromAbuse, http.StatusAccepted))
	admin.AddRoute("/vps/:vpsID/terminate", http.MethodPost, "Résilier un VPS (équipe : récidive grave)", manage, tonic.Handler(r.AdminTerminate, http.StatusAccepted))
	admin.AddRoute("/vps/:vpsID/attach", http.MethodPost, "Rattacher un VPS livré à son identifiant chez le fournisseur", manage, tonic.Handler(r.Attach, http.StatusOK))
	admin.AddRoute("/provider-mails", http.MethodGet, "E-mails du fournisseur reçus par la boîte technique (abus, incidents)", view, tonic.Handler(r.ProviderMails, http.StatusOK))
	admin.AddRoute("/provider-mails/:mailID/handled", http.MethodPost, "Marquer un e-mail du fournisseur comme traité", manage, tonic.Handler(r.HandleProviderMail, http.StatusNoContent))
	admin.AddRoute("/mailbox-check", http.MethodPost, "Diagnostic : la boîte e-mail technique répond (mode compute)", manage, tonic.Handler(r.MailboxCheck, http.StatusAccepted))
	admin.AddRoute("/ping", http.MethodPost, "Diagnostic : une demande traverse outbox, relais, Temporal et mode compute", manage, tonic.Handler(r.Ping, http.StatusAccepted))
	admin.AddRoute("/requests/:requestID", http.MethodGet, "État d'une demande", view, tonic.Handler(r.Request, http.StatusOK))
}
