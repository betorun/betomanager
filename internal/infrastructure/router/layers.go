package router

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	authCtrl "github.com/thekrauss/kubemanager/internal/modules/auth"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	"github.com/thekrauss/kubemanager/internal/modules/builds/images"
	clustersCtrl "github.com/thekrauss/kubemanager/internal/modules/clusters"
	"github.com/thekrauss/kubemanager/internal/modules/copilot"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
	"github.com/thekrauss/kubemanager/internal/modules/iamapi"
	identityCtrl "github.com/thekrauss/kubemanager/internal/modules/identity"
	identitySvc "github.com/thekrauss/kubemanager/internal/modules/identity/service"
	projectCtrl "github.com/thekrauss/kubemanager/internal/modules/projects"
	projectDomain "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	projectSvc "github.com/thekrauss/kubemanager/internal/modules/projects/service"
	realtimeCtrl "github.com/thekrauss/kubemanager/internal/modules/realtime"
	"github.com/thekrauss/kubemanager/internal/modules/sites"
	sitesActs "github.com/thekrauss/kubemanager/internal/modules/sites/activities"
	sitesSvc "github.com/thekrauss/kubemanager/internal/modules/sites/service"
	workloadsCtrl "github.com/thekrauss/kubemanager/internal/modules/workloads"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

func (a *App) initDomainLayers() error {
	a.Logger.Info("Initializing domain layers...")

	policies := &iam.PolicyService{DB: a.DB, Engine: a.IAM}
	rbacService := authSvc.NewRBACService(a.Repos.Auth, a.Logger)
	rbacService.Policies = policies
	apiKeyService := authSvc.NewAPIKeyService(
		a.Repos.Auth,
		authSvc.WithCache(a.AuthCache),
		authSvc.WithEnvTag(a.Config.ServiceName),
	)

	projectService := projectSvc.NewProjectService(
		a.TemporalDistributor,
		a.Config,
		a.Logger,
		a.Repos.Project,
		a.K8sProvider.Client,
		a.K8sProvider.Config, // fix #2 : GetMetrics a besoin du rest.Config
		a.Repos.Workload,     // fix #2 : GetTotalUsageByProject pour calcul usage réservé
	)
	workloadService := workloadsSvc.NewWorkloadService(
		a.TemporalDistributor,
		a.Repos.Workload,
		a.Repos.Project,
		a.K8sProvider.Client,
		a.K8sProvider.Config,
		a.Logger,
	)
	workloadService.Variables = a.Variables
	workloadService.Domains = workloadsRepo.NewDomainStore(a.DB)
	workloadService.Incidents = workloadsRepo.NewIncidentStore(a.DB)
	workloadService.DomainCfg = &workloadsSvc.DomainSettings{
		Resolver:     workloadsSvc.NewResolver(a.Config.Kubernetes.DomainDNSServer),
		ResolverName: workloadsSvc.ResolverName(a.Config.Kubernetes.DomainDNSServer),
		Target:       firstNonEmptyString(a.Config.Kubernetes.IngressTarget, a.Config.Vps.AdressIp),
		TLS:          !a.Config.Kubernetes.CustomDomainInsecure,
		ManagedZone:  a.managedZone,
	}
	// Identité des workloads : cluster des workloads connu et STS en place.
	if a.STS != nil && a.STS.WorkloadClusterID != "" && a.ServiceAccounts != nil {
		workloadService.Identity = &workloadsSvc.IdentitySettings{Authorizer: a.ServiceAccounts,
			Audience: a.STS.Issuer, TokenURI: a.STS.TokenURI()}
	}

	// Identity : dépend de Keycloak + AuthCache (state OAuth). Nil si Keycloak disabled.
	var identityService identitySvc.Service
	var identityController identityCtrl.Controller
	var authController *identityCtrl.AuthController
	if a.Keycloak != nil {
		deps := identitySvc.Deps{
			Keycloak:            a.Keycloak,
			Users:               a.Repos.Auth,
			Projects:            projectService,
			ProjectMembers:      a.Repos.Auth,
			OIDCClientID:        a.Config.Keycloak.ClientID,
			ActionEmailClientID: a.Config.Keycloak.ClientID,
			FrontendBaseURL:     a.Config.Frontend.BaseURL,
			SocialRedirectURI:   a.Config.Keycloak.SocialRedirectURI,
			Cache:               a.AuthCache,
			StateStore:          a.Cache, // Redis : cross-instance OK (fix H9)
			StepUpACR:           firstOrEmpty(a.Config.IAM.MFAACRValues),
			CLIClientID:         a.Config.Keycloak.CLIClientID,
		}
		if err := a.authDeps(&deps); err != nil {
			return err
		}
		identityService = identitySvc.NewService(deps)
		identityController = identityCtrl.NewController(identityService, a.AuthCache)
		authController = identityCtrl.NewAuthController(identityService)
	} else {
		a.Logger.Warnw("identity service skipped — keycloak not initialized")
	}

	rbacController := authCtrl.NewRBACController(rbacService)
	apiKeyController := authCtrl.NewAPIKeyController(apiKeyService)
	var projectAccess projectCtrl.ProjectAccess
	if a.IAM != nil { // pas d'interface non nulle autour d'un *Engine nil
		projectAccess = a.IAM
	}
	projectController := projectCtrl.NewProjectHandlers(projectService, projectAccess)
	workloadController := workloadsCtrl.NewWorkloadHandler(workloadService)

	realtimeController := realtimeCtrl.NewController(a.Config.Centrifugo, a.RealtimeSigner, a.IAM)

	clusterRepo := clustersCtrl.NewClusterRepository(a.DB)
	var clusterOpts []clustersCtrl.Option
	if n := a.alertNotifier(); n != nil {
		clusterOpts = append(clusterOpts, clustersCtrl.WithAlerts(n))
	}
	clustersService := clustersCtrl.NewService(a.Repos.Workload, clusterRepo, a.Publisher, a.Logger, clusterOpts...)
	clustersController := clustersCtrl.NewController(clustersService)

	// Sites statiques : actifs seulement si l'object store est configuré.
	var siteService sitesSvc.ISiteService
	var siteController sites.ISiteController
	if a.ObjectStore != nil {
		sc := a.Config.Sites
		resolver, _ := workloadsSvc.NewResolver(a.Config.Kubernetes.DomainDNSServer).(sitesSvc.DNSResolver)
		concrete := sitesSvc.NewSiteService(a.Repos.Site, a.ObjectStore, a.TemporalDistributor, resolver,
			sitesSvc.Config{
				BaseDomain:  sc.BaseDomain,
				EdgeTarget:  sc.EdgeTarget,
				EdgeIngress: sc.EdgeIngress.Enabled,
				Limits: sitesActs.Limits{
					MaxArchiveBytes:   sc.MaxArchiveBytes,
					MaxExtractedBytes: sc.MaxExtractedBytes,
					MaxFiles:          sc.MaxFiles,
				},
			}, a.Logger)
		siteService = concrete
		concrete.HostnameInUse = func(ctx context.Context, host string) (bool, error) {
			claims, err := workloadService.Domains.HostClaims(ctx, host, uuid.Nil)
			return len(claims) > 0, err
		}
		siteController = sites.NewSiteController(siteService, a.Logger)
	}

	if ps, ok := projectService.(*projectSvc.ProjectService); ok {
		ps.Databases = &databases.GormRepository{DB: a.DB}
	}

	buildController, err := a.newBuildController()
	if err != nil {
		return err
	}
	if buildController != nil {
		workloadService.Registry = &workloadsSvc.RegistrySettings{Host: a.Config.Builds.RegistryHost}
		// Place des images dans les métriques du projet (vue d'ensemble).
		keeper, isKeeper := buildController.Service.Images.(*images.Keeper)
		if ps, ok := projectService.(*projectSvc.ProjectService); ok && isKeeper && keeper != nil {
			ps.Images = keeper
		}
	}

	a.GitHub = a.newGitHubService(buildController, workloadService)
	workloadService.ExternalRegistries = a.Registries
	registriesController := a.newRegistriesController()
	a.WorkloadDeployer = workloadService

	copilotService := a.newCopilotService()

	a.Services = &ServiceContainer{
		Copilot:  copilotService,
		RBAC:     rbacService,
		APIKey:   apiKeyService,
		Project:  projectService,
		Workload: workloadService,
		Identity: identityService,
		Clusters: clustersService,
		Site:     siteService,
	}

	// Facturation : pas de démarrage ni de déploiement pendant une suspension
	// faute de solde, ou dans un projet arrêté par son budget.
	if billing := a.billingService(); billing != nil {
		workloadService.Billing = billing
		// Limites posées à la main par l'équipe : exception au plan, retenue.
		projectController.OnLimitsOverride = func(ctx context.Context, projectID string, l projectDomain.UsageDTO, by string) error {
			return billing.RecordOverride(ctx, projectID, l.CPU, l.Memory, l.Storage, by)
		}
	}

	a.Controllers = &ControllerContainer{
		Builds:      buildController,
		GitHub:      a.newGitHubController(a.GitHub),
		Registries:  registriesController,
		Billing:     a.newBillingController(),
		DomainSales: a.newDomainSalesController(),
		RBAC:        rbacController,
		APIKey:      apiKeyController,
		Project:     projectController,
		Workload:    workloadController,
		Identity:    identityController,
		Auth:        authController,
		Realtime:    realtimeController,
		Clusters:    clustersController,
		Site:        siteController,
		IAM:         a.newIAMController(policies),
		Copilot:     copilot.NewController(copilotService),
		Databases:   a.newDatabasesController(),
	}

	a.Logger.Info("Domain layers successfully initialized.")
	return nil
}

func firstOrEmpty(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

func (a *App) newIAMController(policies *iam.PolicyService) *iamapi.Controller {
	ctrl := iamapi.NewController(policies, a.IAM, a.ServiceAccounts, a.ClusterAgents, a.STS)
	ctrl.OrgPolicies = a.OrgPolicies
	ctrl.MFA = iamapi.MFASettings{ACRValues: a.Config.IAM.MFAACRValues, MaxAge: a.stepUpMaxAge()}
	return ctrl
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
