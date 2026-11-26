package router

import (
	"github.com/thekrauss/kubemanager/internal/modules/addons"
	"github.com/thekrauss/kubemanager/internal/modules/alerts"
	"github.com/thekrauss/kubemanager/internal/modules/assistance"
	controller "github.com/thekrauss/kubemanager/internal/modules/auth"
	authRepos "github.com/thekrauss/kubemanager/internal/modules/auth/repository"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	"github.com/thekrauss/kubemanager/internal/modules/billing"
	"github.com/thekrauss/kubemanager/internal/modules/builds"
	"github.com/thekrauss/kubemanager/internal/modules/clusters"
	clustersSvc "github.com/thekrauss/kubemanager/internal/modules/clusters/service"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
	"github.com/thekrauss/kubemanager/internal/modules/copilot"
	copilotSvc "github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
	"github.com/thekrauss/kubemanager/internal/modules/diskresize"
	"github.com/thekrauss/kubemanager/internal/modules/diskusage"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales"
	"github.com/thekrauss/kubemanager/internal/modules/egress"
	"github.com/thekrauss/kubemanager/internal/modules/github"
	"github.com/thekrauss/kubemanager/internal/modules/iamapi"
	identity "github.com/thekrauss/kubemanager/internal/modules/identity"
	identitySvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
	"github.com/thekrauss/kubemanager/internal/modules/insights"
	"github.com/thekrauss/kubemanager/internal/modules/legal"
	mailmod "github.com/thekrauss/kubemanager/internal/modules/mail"
	"github.com/thekrauss/kubemanager/internal/modules/network"
	"github.com/thekrauss/kubemanager/internal/modules/operatoraccess"
	"github.com/thekrauss/kubemanager/internal/modules/projects"
	projectRepos "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	projectSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
	realtimeCtrl "github.com/thekrauss/kubemanager/internal/modules/realtime"
	"github.com/thekrauss/kubemanager/internal/modules/registries"
	"github.com/thekrauss/kubemanager/internal/modules/sites"
	sitesRepo "github.com/thekrauss/kubemanager/internal/modules/sites/repository"
	sitesSvc "github.com/thekrauss/kubemanager/internal/modules/sites/service"
	"github.com/thekrauss/kubemanager/internal/modules/status"
	"github.com/thekrauss/kubemanager/internal/modules/storages3"
	"github.com/thekrauss/kubemanager/internal/modules/support"
	"github.com/thekrauss/kubemanager/internal/modules/vpc"
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
	Clusters clustersSvc.IClusterService
	Site     sitesSvc.ISiteService // nil si sites désactivés
	// Copilot : toujours présent ; LLM nil = désactivé.
	Copilot *copilotSvc.Service
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
	Clusters clusters.IClusterController
	Site     sites.ISiteController // nil si sites désactivés
	IAM      *iamapi.Controller
	Builds   *builds.Controller // nil si builds désactivés
	GitHub   *github.Controller
	// Registries : accès des projets aux registres d'images externes.
	Registries *registries.Controller
	Copilot    *copilot.Controller
	Databases  *databases.Controller
	// StorageS3 : stockage objet des clients (ADR 0015).
	StorageS3 *storages3.Controller
	Network   *network.Controller
	Egress    *egress.Controller
	// VPC : réseau privé du projet étendu à ses VPS (ADR 0019).
	VPC *vpc.Controller
	// Addons : services additionnels (Redis via Valkey…).
	Addons *addons.Controller
	// DiskUsage : usage réel des disques des projets.
	DiskUsage *diskusage.Controller
	// Insights : « L'assistant a remarqué » (constats d'un projet).
	Insights *insights.Controller
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
	// Compute : VPS (offres, réglage, diagnostic).
	Compute *compute.Controller
	// Legal : conditions de vente et politique d'usage.
	Legal *legal.Controller
	// Status : page de statut publique et incidents.
	Status *status.Controller
	// Support : tickets des clients, file de l'équipe.
	Support *support.Controller
	// OperatorAccess : jeton de l'équipe pour l'Opérateur (ADR 0016).
	OperatorAccess *operatoraccess.Controller
	// Assistance : accès d'assistance de l'équipe chez un client (ADR 0017).
	Assistance *assistance.Controller
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
	addStorageS3Routes(a)
	addNetworkRoutes(a)
	addEgressRoutes(a)
	addVPCRoutes(a)
	addAddonRoutes(a)
	addDiskResizeRoutes(a)
	addAlertPreferenceRoutes(a)
	addBillingRoutes(a)
	addDomainSalesRoutes(a)
	addMailRoutes(a)
	addComputeRoutes(a)
	addLegalRoutes(a)
	addSupportRoutes(a)
	addStatusRoutes(a)
	addOperatorRoutes(a)
	addAssistanceRoutes(a)
	addInsightsRoutes(a)
	// Copilot : outils et guides de la table de couverture (copilot_decisions.go).
	applyCopilotDecisions(AllGroups)
	// Opérateur : outils de lecture de la table operator_decisions.go.
	applyOperatorDecisions(AllGroups)
}
