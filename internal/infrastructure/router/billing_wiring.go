package router

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/thekrauss/kubemanager/internal/modules/billing"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	"github.com/thekrauss/kubemanager/internal/modules/billing/xykpay"
	projectDomain "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	projectSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
)

// billingService : facturation (ADR 0012). Même service pour l'API
// (estimation) et le worker (relevé de l'usage).
func (a *App) billingService() *billingSvc.Service {
	if a.DB == nil {
		return nil
	}
	svc := &billingSvc.Service{
		Store:     billingRepo.New(a.DB),
		Inventory: billingInventory.New(a.DB),
		Ledger:    billingRepo.NewLedger(a.DB),
		Admin:     billingRepo.NewAdmin(a.DB),
		Logger:    a.Logger.With("component", "billing"),
	}
	// Recharges : seulement si le compte XYKPay est complet (clé et secret
	// des webhooks) ; sinon la console les montre fermées.
	if x := a.Config.Billing.XYKPay; x.BaseURL != "" && x.APIKey != "" && x.WebhookSecret != "" {
		svc.Payments = xykpay.New(x.BaseURL, x.APIKey)
		svc.WebhookSecret = x.WebhookSecret
	}
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		svc.Alerts = n
		svc.Budgets = n
		svc.Reminders = n
	}
	// Phase 4 : suspension (arrêt et relance des conteneurs), documents.
	svc.Phase4 = billingRepo.NewPhase4(a.DB)
	if a.Services != nil {
		if c, ok := a.Services.Workload.(billingSvc.Containers); ok {
			svc.Containers = c
		}
	}
	// Phase 5 : plans ; le changement de plan passe par le service des projets
	// (base, puis quota Kubernetes).
	svc.Plans = billingRepo.NewPlans(a.DB)
	svc.Postpaid = billingRepo.NewPostpaid(a.DB)
	// Suppression des données après la suspension : l'état (console) ; la
	// boucle tourne dans le processus API (startSuspensionPurge).
	svc.Purges = billingRepo.NewPurges(a.DB)
	if a.Services != nil {
		if ps, ok := a.Services.Project.(*projectSvc.ProjectService); ok && ps != nil {
			svc.Limits = projectLimiter{ps}
		}
	}
	is := a.Config.Billing.Issuer
	svc.Issuer = billingDomain.Issuer{Name: is.Name, Address: is.Address, TaxID: is.TaxID, Country: is.Country,
		VATRateBP: is.VATRateBP, PaymentInstructions: is.PaymentInstructions, InvoicePrefix: or(is.InvoicePrefix, "BTR"), ReceiptPrefix: or(is.ReceiptPrefix, "REC")}
	return svc
}

func (a *App) newBillingController() *billing.Controller {
	svc := a.billingService()
	if svc == nil {
		return nil
	}
	return &billing.Controller{Service: svc}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// projectLimiter : limites d'un projet par le service des projets.
type projectLimiter struct{ svc *projectSvc.ProjectService }

func (l projectLimiter) SetLimits(ctx context.Context, project uuid.UUID, cpu, memory, storage string) error {
	_, err := l.svc.SetLimits(ctx, project.String(), projectDomain.ProjectLimits{CPU: cpu, Memory: memory, Storage: storage})
	if errors.Is(err, projectSvc.ErrLimitsBelowUsage) {
		return belowUsage{err}
	}
	return err
}

// belowUsage : refus « limites sous l'existant », reconnu par la facturation.
type belowUsage struct{ error }

func (b belowUsage) BelowUsage() bool { return true }
func (b belowUsage) Unwrap() error    { return b.error }
