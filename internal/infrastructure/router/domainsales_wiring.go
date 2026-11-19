package router

import (
	"context"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/domains/registrar"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales"
	domainsalesRepo "github.com/thekrauss/kubemanager/internal/modules/domainsales/repository"
	domainsalesSvc "github.com/thekrauss/kubemanager/internal/modules/domainsales/service"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// domainSalesService : revente de domaines (registraire OVH). Sans
// identifiants OVH : fermée ; sans ORDERING_ENABLED : recherche seulement.
func (a *App) domainSalesService() *domainsalesSvc.Service {
	billing := a.billingService()
	if a.DB == nil || billing == nil {
		return nil
	}
	svc := &domainsalesSvc.Service{Store: domainsalesRepo.New(a.DB), Wallet: billing, Logger: a.Logger.With("component", "domain-sales")}
	o := a.Config.Domains.OVH
	if o.ApplicationKey != "" {
		reg, err := registrar.NewOVH(registrar.OVHConfig{Endpoint: o.Endpoint, ApplicationKey: o.ApplicationKey,
			ApplicationSecret: o.ApplicationSecret, ConsumerKey: o.ConsumerKey, Subsidiary: o.Subsidiary, OrderingEnabled: o.OrderingEnabled})
		if err != nil {
			a.Logger.Warnw("domain sales disabled: OVH configuration incomplete", "error", err)
		} else {
			svc.Registrar = reg
			svc.Ordering = o.OrderingEnabled
		}
	}
	if a.Services != nil {
		if ws, ok := a.Services.Workload.(*workloadsSvc.WorkloadService); ok && ws != nil {
			svc.Workloads = workloadDomains{ws: ws, app: a}
		}
	}
	return svc
}

func (a *App) newDomainSalesController() *domainsales.Controller {
	svc := a.domainSalesService()
	if svc == nil {
		return nil
	}
	return &domainsales.Controller{Service: svc}
}

// workloadDomains : domaines des conteneurs, pour brancher un domaine acheté.
type workloadDomains struct {
	ws  *workloadsSvc.WorkloadService
	app *App
}

func (w workloadDomains) WorkloadOrg(ctx context.Context, workload uuid.UUID) (uuid.UUID, error) {
	var ids []uuid.UUID
	err := w.app.DB.WithContext(ctx).Raw(`SELECT p.organization_id FROM workloads wl JOIN projects p ON p.id = wl.project_id
		WHERE wl.id = ? AND wl.status <> 'DELETED'`, workload).Scan(&ids).Error
	if err != nil || len(ids) == 0 {
		return uuid.Nil, workloadsDomain.ErrDomainNotFound
	}
	return ids[0], nil
}

func (w workloadDomains) AddDomain(ctx context.Context, workload uuid.UUID, hostname, path string) (*workloadsDomain.DomainResponse, error) {
	return w.ws.AddDomain(ctx, workload, hostname, path)
}

func (w workloadDomains) VerifyDomain(ctx context.Context, workload, id uuid.UUID) (*workloadsDomain.DomainResponse, error) {
	return w.ws.VerifyDomain(ctx, workload, id)
}

// managedZone : domaine vendu par la plateforme (actif ou en commande).
func (a *App) managedZone(ctx context.Context, zone string) bool {
	var n int64
	err := a.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM domain_orders WHERE domain = ? AND status IN ('ordering', 'placed', 'active')`, zone).
		Scan(&n).Error
	return err == nil && n > 0
}
