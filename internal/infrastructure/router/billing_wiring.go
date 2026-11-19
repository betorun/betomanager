package router

import (
	"github.com/thekrauss/kubemanager/internal/modules/billing"
	billingInventory "github.com/thekrauss/kubemanager/internal/modules/billing/inventory"
	billingRepo "github.com/thekrauss/kubemanager/internal/modules/billing/repository"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
)

// billingService : facturation (ADR 0012). Même service pour l'API
// (estimation) et le worker (relevé de l'usage).
func (a *App) billingService() *billingSvc.Service {
	if a.DB == nil {
		return nil
	}
	return &billingSvc.Service{
		Store:     billingRepo.New(a.DB),
		Inventory: billingInventory.New(a.DB),
		Logger:    a.Logger.With("component", "billing"),
	}
}

func (a *App) newBillingController() *billing.Controller {
	svc := a.billingService()
	if svc == nil {
		return nil
	}
	return &billing.Controller{Service: svc}
}
