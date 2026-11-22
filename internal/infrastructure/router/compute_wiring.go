package router

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
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
	s := &computeSvc.Service{Store: &computeRepo.GormStore{DB: a.DB}}
	if billing := a.billingService(); billing != nil {
		s.Catalogs = computeCatalogs{billing: billing}
	}
	return s
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
	if provider != nil {
		activities.Provider = provider // interface nil si aucune clé
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
		ApplicationSecret: c.ApplicationSecret, ConsumerKey: c.ConsumerKey})
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
