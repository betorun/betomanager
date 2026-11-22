package router

import (
	"github.com/thekrauss/kubemanager/internal/modules/addons"
	"github.com/thekrauss/kubemanager/internal/modules/alerts"
	controller "github.com/thekrauss/kubemanager/internal/modules/auth"
	authRepos "github.com/thekrauss/kubemanager/internal/modules/auth/repository"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	"github.com/thekrauss/kubemanager/internal/modules/billing"
	"github.com/thekrauss/kubemanager/internal/modules/builds"
	clustersCtrl "github.com/thekrauss/kubemanager/internal/modules/clusters"
	"github.com/thekrauss/kubemanager/internal/modules/copilot"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
	"github.com/thekrauss/kubemanager/internal/modules/diskresize"
	"github.com/thekrauss/kubemanager/internal/modules/diskusage"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales"
	"github.com/thekrauss/kubemanager/internal/modules/github"
	"github.com/thekrauss/kubemanager/internal/modules/iamapi"
	identity "github.com/thekrauss/kubemanager/internal/modules/identity"
	identitySvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
	mailmod "github.com/thekrauss/kubemanager/internal/modules/mail"
	"github.com/thekrauss/kubemanager/internal/modules/projects"
	projectRepos "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	projectSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
	realtimeCtrl "github.com/thekrauss/kubemanager/internal/modules/realtime"
	"github.com/thekrauss/kubemanager/internal/modules/registries"
	"github.com/thekrauss/kubemanager/internal/modules/sites"
	sitesRepo "github.com/thekrauss/kubemanager/internal/modules/sites/repository"
	sitesSvc "github.com/thekrauss/kubemanager/internal/modules/sites/service"
	workload "github.com/thekrauss/kubemanager/internal/modules/workloads"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

type RepositoryContainer struct {
	Auth     authRepos.AuthRepository
	Project  projectRepos.ProjectRepository
	Workload workloadsRepo.WorkloadRepository
	Site     sitesRepo.SiteRepository
}

type ServiceContainer struct {
	RBAC     authSvc.IRBACService
	APIKey   authSvc.IAPIKeyService
	Project  projectSvc.IProjectService
	Workload workloadsSvc.IWorkloadService
	Identity identitySvc.Service
	Clusters clustersCtrl.IClusterService
	Site     sitesSvc.ISiteService // nil si sites désactivés
	// Copilot : toujours présent ; LLM nil = désactivé.
	Copilot *copilot.Service
}

type ControllerContainer struct {
	APIKey   controller.IAPIKeyController
	RBAC     controller.IRBACController
	Project  projects.IProjectController
	Workload workload.IWorkloadController
	Identity identity.Controller
	// Auth : connexion, sessions, second facteur, emails (nil sans annuaire).
	Auth     *identity.AuthController
	Realtime realtimeCtrl.IRealtimeController
	Clusters clustersCtrl.IClusterController
	Site     sites.ISiteController // nil si sites désactivés
	IAM      *iamapi.Controller
	Builds   *builds.Controller // nil si builds désactivés
	GitHub   *github.Controller
	// Registries : accès des projets aux registres d'images externes.
	Registries *registries.Controller
	Copilot    *copilot.Controller
	Databases  *databases.Controller
	// Addons : services additionnels (Redis via Valkey…).
	Addons *addons.Controller
	// DiskUsage : usage réel des disques des projets.
	DiskUsage *diskusage.Controller
	// DiskResize : agrandissement des disques par copie.
	DiskResize *diskresize.Controller
	// Alerts : préférences d'e-mail et désabonnement.
	Alerts *alerts.Controller
	// Billing : facturation (coût estimé, grille de prix).
	Billing *billing.Controller
	// DomainSales : revente de noms de domaine.
	DomainSales *domainsales.Controller
	// Mail : webhook SendGrid et adresses mises de côté.
	Mail *mailmod.Controller
}

func AddAllRoutes(a *App) {
	addIdentityRoutes(a)
	addRBACRoutes(a)
	addAPIKeyRoutes(a)
	addProjectRoutes(a)
	addWorkloadRoutes(a)
	addRealtimeRoutes(a)
	addClusterRoutes(a)
	addSiteRoutes(a)
	addBuildRoutes(a)
	addGitHubRoutes(a)
	addRegistriesRoutes(a)
	addIAMRoutes(a)
	addCopilotRoutes(a)
	addDatabaseRoutes(a)
	addAddonRoutes(a)
	addDiskResizeRoutes(a)
	addAlertPreferenceRoutes(a)
	addBillingRoutes(a)
	addDomainSalesRoutes(a)
	addMailRoutes(a)
}
