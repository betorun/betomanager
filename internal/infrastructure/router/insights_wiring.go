package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	addonsSvc "github.com/thekrauss/kubemanager/internal/modules/addons/service"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	buildsSvc "github.com/thekrauss/kubemanager/internal/modules/builds/service"
	databasesRepo "github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	diskusageSvc "github.com/thekrauss/kubemanager/internal/modules/diskusage/service"
	"github.com/thekrauss/kubemanager/internal/modules/insights"
	insightsRepo "github.com/thekrauss/kubemanager/internal/modules/insights/repository"
	insightsSvc "github.com/thekrauss/kubemanager/internal/modules/insights/service"
	projectRepos "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	sitesRepo "github.com/thekrauss/kubemanager/internal/modules/sites/repository"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// newInsightsController : constats d'un projet. Chaque source est servie par
// le module qui possède la donnée ; un module désactivé = source omise.
// Appelé une fois les autres contrôleurs construits (il réutilise leurs services).
func (a *App) newInsightsController() *insights.Controller {
	if a.Repos == nil || a.Repos.Workload == nil || a.DB == nil {
		return nil
	}
	s := &insightsSvc.Service{
		Workloads:       &insightWorkloads{repo: a.Repos.Workload},
		WorkloadDomains: &insightDomains{store: workloadsRepo.NewDomainStore(a.DB)},
		Databases:       &insightDatabases{repo: &databasesRepo.GormRepository{DB: a.DB}},
	}
	if a.Logger != nil {
		s.Logger = a.Logger.With("component", "insights")
	}
	if a.Services != nil && a.Services.Workload != nil {
		s.Workloads.(*insightWorkloads).svc = a.Services.Workload
	}
	if a.Repos.Site != nil {
		s.Sites = &insightSites{repo: a.Repos.Site}
	}
	if a.Repos.Project != nil {
		s.Projects = &insightProjects{repo: a.Repos.Project}
	}
	if a.Redis != nil {
		s.Cache = &insightsRepo.RedisCache{Client: a.Redis}
	}
	if c := a.Controllers; c != nil {
		if c.Builds != nil && c.Builds.Service != nil {
			s.Builds = &insightBuilds{svc: c.Builds.Service}
		}
		if c.Billing != nil && c.Billing.Service != nil {
			s.Budgets = &insightBudgets{svc: c.Billing.Service}
		}
		if c.DiskUsage != nil && c.DiskUsage.Service != nil {
			s.Disks = &insightDisks{svc: c.DiskUsage.Service}
		}
		if c.Addons != nil && c.Addons.Service != nil {
			s.Addons = &insightAddons{svc: c.Addons.Service}
		}
	}
	ctl := &insights.Controller{Service: s}
	if a.IAM != nil { // pas d'interface non nulle autour d'un *Engine nil
		ctl.IAM = a.IAM
	}
	return ctl
}

func addInsightsRoutes(app *App) {
	ctl := app.Controllers.Insights
	if ctl == nil {
		return
	}
	g := newGuards(app)
	ProjectGroup.AddRoute("/:id/insights", http.MethodGet,
		"Ce que l'assistant a remarqué : conteneurs en échec, builds ratés, budget, disques, domaines, bases et services (selon vos droits ; recalculé toutes les 60 s)",
		g.Perm(iam.ProjectsGet, security.ProjectParam("id")), tonic.Handler(ctl.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_project_insights",
			Hint: "What needs attention in the project right now (failing containers, failed builds, budget, full disks, unverified domains, stopped databases, unhealthy services), limited to what the person may read. Use diagnose_workload on a failing container for the cause."})
}

// ── Adaptateurs : chaque source depuis son module ───────────────────────────

type insightWorkloads struct {
	repo workloadsRepo.WorkloadRepository
	svc  workloadsSvc.IWorkloadService
}

func (w *insightWorkloads) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.WorkloadState, error) {
	list, err := w.repo.ListByProject(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.WorkloadState, 0, len(list))
	for _, wl := range list {
		out = append(out, insightsSvc.WorkloadState{ID: wl.ID, Name: wl.Name, Kind: wl.Kind, Status: wl.Status,
			ManagedBy: wl.ManagedBy, UpdatedAt: wl.UpdatedAt})
	}
	return out, nil
}

// Restarts : redémarrages lus en direct (somme des instances).
func (w *insightWorkloads) Restarts(ctx context.Context, id uuid.UUID) (int, error) {
	if w.svc == nil {
		return 0, nil
	}
	st, err := w.svc.LiveStatus(ctx, id)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range st.Pods {
		n += int(p.RestartCount)
	}
	return n, nil
}

type insightBuilds struct{ svc *buildsSvc.Service }

func (b *insightBuilds) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.BuildState, error) {
	list, err := b.svc.List(ctx, project, "")
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.BuildState, 0, len(list))
	for _, x := range list {
		out = append(out, insightsSvc.BuildState{ID: x.ID, Name: x.Name, Status: x.Status, Failure: x.Failure,
			WorkloadID: x.DeployWorkloadID, CreatedAt: x.CreatedAt, FinishedAt: x.FinishedAt})
	}
	return out, nil
}

type insightBudgets struct{ svc *billingSvc.Service }

func (b *insightBudgets) Budget(ctx context.Context, project uuid.UUID) (*insightsSvc.BudgetState, error) {
	v, err := b.svc.Budget(ctx, project)
	if err != nil || v == nil {
		return nil, err
	}
	return &insightsSvc.BudgetState{Limit: v.Limit, Spent: v.Spent, Stopped: v.Stopped}, nil
}

type insightDisks struct{ svc *diskusageSvc.Service }

func (d *insightDisks) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.DiskState, error) {
	list, err := d.svc.List(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.DiskState, 0, len(list.Disks))
	for _, u := range list.Disks {
		out = append(out, insightsSvc.DiskState{Kind: u.Kind, TargetID: u.TargetID, TargetName: u.TargetName,
			Used: u.UsedBytes, Capacity: u.CapacityBytes, MeasuredAt: u.MeasuredAt})
	}
	return out, nil
}

type insightDomains struct{ store workloadsRepo.DomainStore }

func (d *insightDomains) List(ctx context.Context, workload uuid.UUID) ([]insightsSvc.DomainState, error) {
	list, err := d.store.List(ctx, workload)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.DomainState, 0, len(list))
	for _, x := range list {
		out = append(out, insightsSvc.DomainState{ID: x.ID, Hostname: x.Hostname, Verified: x.Verified, CreatedAt: x.CreatedAt})
	}
	return out, nil
}

type insightSites struct{ repo sitesRepo.SiteRepository }

func (s *insightSites) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.SiteState, error) {
	list, err := s.repo.ListSites(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.SiteState, 0, len(list))
	for _, x := range list {
		out = append(out, insightsSvc.SiteState{ID: x.ID, Name: x.Name})
	}
	return out, nil
}

func (s *insightSites) Domains(ctx context.Context, site uuid.UUID) ([]insightsSvc.DomainState, error) {
	list, err := s.repo.ListDomains(ctx, site)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.DomainState, 0, len(list))
	for _, x := range list {
		out = append(out, insightsSvc.DomainState{ID: x.ID, Hostname: x.Hostname, Verified: x.Verified, CreatedAt: x.CreatedAt})
	}
	return out, nil
}

type insightDatabases struct{ repo *databasesRepo.GormRepository }

func (d *insightDatabases) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.DatabaseState, error) {
	list, err := d.repo.List(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.DatabaseState, 0, len(list))
	for _, x := range list {
		st := insightsSvc.DatabaseState{ID: x.ID, Name: x.Name, Status: x.Status, UpdatedAt: x.UpdatedAt}
		if x.Status == "STOPPED" { // branchements : seulement pour une base arrêtée
			at, err := d.repo.ListAttachments(ctx, x.ID)
			if err != nil {
				return nil, err
			}
			st.Attached = len(at)
		}
		out = append(out, st)
	}
	return out, nil
}

type insightAddons struct{ svc *addonsSvc.Service }

func (a *insightAddons) List(ctx context.Context, project uuid.UUID) ([]insightsSvc.AddonState, error) {
	list, err := a.svc.List(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]insightsSvc.AddonState, 0, len(list.Addons))
	for _, x := range list.Addons {
		out = append(out, insightsSvc.AddonState{ID: x.ID, Name: x.Name, Status: x.Status})
	}
	return out, nil
}

type insightProjects struct {
	repo projectRepos.ProjectRepository
}

func (p *insightProjects) Name(ctx context.Context, project uuid.UUID) (string, error) {
	pr, err := p.repo.GetProjectByID(ctx, project.String())
	if err != nil {
		return "", err
	}
	return pr.Name, nil
}
