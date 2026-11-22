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
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
	computeRepo "github.com/thekrauss/kubemanager/internal/modules/compute/repository"
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
	w := worker.New(a.Temporal.Client, a.computeTaskQueue(), worker.Options{})
	w.RegisterWorkflowWithOptions(computeWorkflows.RequestWorkflow, workflow.RegisterOptions{Name: computeWorkflows.RequestWorkflowName})
	w.RegisterActivity(&computeWorkflows.Activities{Store: &computeRepo.GormStore{DB: a.DB}})
	if err := w.Start(); err != nil {
		return fmt.Errorf("compute worker: %w", err)
	}
	defer w.Stop()
	a.Logger.Infow("compute worker started", "task_queue", a.computeTaskQueue())
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
	view := g.Perm(iam.PlatformBillingCatalogsGet, security.PlatformResource)
	edit := g.Perm(iam.PlatformBillingCatalogsUpdate, security.PlatformResource)
	admin.AddRoute("/offers", http.MethodGet, "Offres de VPS (prix fournisseur, coefficient, emplacements, systèmes)", view, tonic.Handler(r.AdminOffers, http.StatusOK))
	admin.AddRoute("/offers/:code", http.MethodPut, "Régler une offre de VPS (prix fournisseur, coefficient, palier)", edit, tonic.Handler(r.SaveOffer, http.StatusOK))
	admin.AddRoute("/ping", http.MethodPost, "Diagnostic : une demande traverse outbox, relais, Temporal et mode compute", edit, tonic.Handler(r.Ping, http.StatusAccepted))
	admin.AddRoute("/requests/:requestID", http.MethodGet, "État d'une demande", view, tonic.Handler(r.Request, http.StatusOK))
}
