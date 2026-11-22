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

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
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
		AccountCatalogRate(ctx context.Context, org uuid.UUID) (int64, string, error)
	}
}

func (c computeCatalogs) EURRate(ctx context.Context, org uuid.UUID) (int64, string, error) {
	return c.billing.AccountCatalogRate(ctx, org)
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
	}
	if billing := a.billingService(); billing != nil {
		activities.Wallet = billing
	}
	if e := a.computeEvents(); e != nil {
		activities.Events = e
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
	admin.AddRoute("/ping", http.MethodPost, "Diagnostic : une demande traverse outbox, relais, Temporal et mode compute", manage, tonic.Handler(r.Ping, http.StatusAccepted))
	admin.AddRoute("/requests/:requestID", http.MethodGet, "État d'une demande", view, tonic.Handler(r.Request, http.StatusOK))
}
