package router

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/thekrauss/kubemanager/internal/infrastructure/workers/queue"
	clustersSvc "github.com/thekrauss/kubemanager/internal/modules/clusters/service"
	egressSvc "github.com/thekrauss/kubemanager/internal/modules/egress/service"
	storageActivities "github.com/thekrauss/kubemanager/internal/modules/storages3/activities"
	vpcSvc "github.com/thekrauss/kubemanager/internal/modules/vpc/service"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thekrauss/kubemanager/internal/auth/sso"
	addonsSvc "github.com/thekrauss/kubemanager/internal/modules/addons/service"
	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	diskusageSvc "github.com/thekrauss/kubemanager/internal/modules/diskusage/service"
	domainsalesSvc "github.com/thekrauss/kubemanager/internal/modules/domainsales/service"
	registriesRepo "github.com/thekrauss/kubemanager/internal/modules/registries/repository"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	buildActs "github.com/thekrauss/kubemanager/internal/modules/builds/activities"
	githubSvc "github.com/thekrauss/kubemanager/internal/modules/github/service"

	"github.com/redis/go-redis/v9"
	"github.com/thekrauss/beto-shared/pkg/logger"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/core/cache"
	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/core/workerpool"
	idempotency "github.com/thekrauss/kubemanager/internal/infrastructure/idempotence"
	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/infrastructure/realtime"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	identityKC "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
)

type Mode string

const (
	ModeServer Mode = "server"
	ModeWorker Mode = "worker"
	// ModePlatform : API du back-office de l'équipe betorun (routes /admin),
	// hébergée à part de l'API des clients (docs/architecture/platform-console.md).
	ModePlatform Mode = "platform"
	// ModeCompute : worker Temporal des VPS (file « compute »), seul process
	// qui charge les clés du fournisseur (docs/modules/compute.md).
	ModeCompute Mode = "compute"
	EnvAppMode       = "APP_MODE"
)

func ResolveMode() Mode {
	switch Mode(os.Getenv(EnvAppMode)) {
	case ModeWorker:
		return ModeWorker
	case ModePlatform:
		return ModePlatform
	case ModeCompute:
		return ModeCompute
	default:
		return ModeServer
	}
}

type App struct {
	Config *configs.GlobalConfig
	Logger *zap.SugaredLogger

	DB          *gorm.DB
	Redis       *redis.Client
	K8sProvider *k8sprovider.ProviderK8s
	// clusters : registre des clusters (clusterRegistry, construit une fois).
	clusters *clustersSvc.Registry
	// kubeResolver : client du cluster de chaque projet (kube()).
	kubeResolver k8sprovider.Resolver
	// egress : IP de sortie dédiée (ADR 0014), partagé API et worker.
	egress *egressSvc.Service
	// vpc : réseau privé des projets étendu à leurs VPS (ADR 0019).
	vpc                 *vpcSvc.Service
	Cache               cache.CacheRedis
	AuthCache           cache.AuthCache
	Temporal            TemporalComponents
	TemporalDistributor temporal.TaskDistributor
	Keycloak            identityKC.KeycloakClient
	Idempotency         idempotency.AtomicManager
	AsyncPool           *workerpool.Pool          // updates DB async
	UsageAccumulator    *authSvc.UsageAccumulator // batch API key usage
	Publisher           realtime.Publisher
	RealtimeSigner      realtime.TokenSigner
	ObjectStore         objectstore.Store // nil si sites désactivés
	Variables           workloadsRepo.VariableStore
	Registries          registriesRepo.Store
	IAM                 *iam.Engine
	ServiceAccounts     *iam.ServiceAccountService
	ClusterAgents       *iam.ClusterAgents
	STS                 *sts.Service
	OrgPolicies         *iam.OrgPolicyService
	// mailQueue : file des e-mails (asynq sur Redis), créée au premier envoi.
	mailQueue *queue.RedisTaskDistributor
	// SSO : connexion d'entreprise des organisations (nil sans authentification betorun).
	SSO *sso.Store
	// GitHub : connexion GitHub (Client nil : désactivée).
	GitHub *githubSvc.Service
	// WorkloadDeployer : mise à jour d'un conteneur à la fin d'un build (worker).
	WorkloadDeployer buildActs.Deployer

	Security      Security
	Servers       Servers
	Observability Observability

	Repos       *RepositoryContainer
	Services    *ServiceContainer
	Controllers *ControllerContainer

	// life : contexte de vie et suivi des tâches de fond (lifecycle.go).
	life lifecycle
}

func NewApp(cfg *configs.GlobalConfig) *App {
	log := logger.InitLogger(cfg.Logger)
	return &App{Config: cfg, Logger: log}
}

func (a *App) Init(ctx context.Context) error {
	a.life.init(ctx)
	if err := a.initDependencies(); err != nil {
		return fmt.Errorf("dependency init: %w", err)
	}
	if err := a.initDomainLayers(); err != nil {
		return fmt.Errorf("domain init: %w", err)
	}
	return nil
}

func (a *App) RunServer(ctx context.Context) error {
	defer a.shutdownInfra()

	a.startHTTPServer(ModeServer)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Données des organisations suspendues trop longtemps (ADR 0012).
	a.startSuspensionPurge(ctx)
	// Projets dont le délai de grâce est écoulé : effacement définitif.
	a.startProjectPurge(ctx)
	a.startStorageUsage(ctx)
	<-ctx.Done()

	a.Logger.Infow("server shutdown signal received")
	a.gracefulShutdownServers(a.Servers.GRPC, a.Servers.HTTP, a.Config.Server.ShutdownTimeout)
	a.Logger.Infow("server stopped gracefully")
	return nil
}

// RunPlatform : API du back-office (routes /admin, connexion, IAM). Aucune
// boucle de fond : elles tournent dans l'API des clients et le worker.
func (a *App) RunPlatform(ctx context.Context) error {
	defer a.shutdownInfra()

	a.startHTTPServer(ModePlatform)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	a.Logger.Infow("platform API shutdown signal received")
	a.gracefulShutdownServers(a.Servers.GRPC, a.Servers.HTTP, a.Config.Server.ShutdownTimeout)
	return nil
}

func (a *App) RunWorker(ctx context.Context) error {
	defer a.shutdownInfra()

	processor := temporal.NewTemporalProcessor(temporal.WorkerConfig{
		Client:      a.Temporal.Client,
		Config:      a.Config,
		Logger:      a.Logger,
		K8sClient:   a.K8sProvider.Client,
		Kube:        a.kube(),
		DB:          a.DB,
		RBACSvc:     a.Services.RBAC,
		Idempotency: a.Idempotency,
		Publisher:   a.Publisher,
		ObjectStore: a.ObjectStore,
		Builds:      a.buildActivities(),
		Pull:        a.pullSecrets(),
		Variables:   a.Variables,
		Storage:     &storageActivities.StorageActivities{Purger: a.storagePurger()},
	})

	errCh, err := processor.Start()
	if err != nil {
		return fmt.Errorf("temporal processor start: %w", err)
	}
	a.Temporal.Worker = processor.Worker()
	a.Logger.Infow("temporal worker started", "task_queue", a.Config.Temporal.TaskQueue)

	healthSrv := a.startHealthServer()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Retour à RUNNING après un incident signalé par le cluster.
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		recoverer := &workloadsSvc.Recoverer{
			Store:     workloadsRepo.NewIncidentStore(a.DB),
			K8s:       k8sprovider.ByProject(a.kube()),
			Publisher: a.Publisher,
			Logger:    a.Logger.With("component", "workload-recovery"),
		}
		if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
			recoverer.Alerts = n
		}
		a.goWith(ctx, "workload-recovery", func(ctx context.Context) { recoverer.Run(ctx, workloadsSvc.DefaultRecoverEvery) })
	}

	// Facturation : relevé de l'usage réservé (ADR 0012, phase 2 à blanc).
	if billing := a.billingService(); billing != nil {
		a.goWith(ctx, "billing-sample", func(ctx context.Context) { billing.Run(ctx, billingSvc.DefaultSampleEvery) })
		// Phase 3 : débit de l'usage (organisations activées), recharges en
		// attente, crédits expirés, alertes de solde.
		a.goWith(ctx, "billing-wallet", func(ctx context.Context) { billing.RunWallet(ctx, billingSvc.DefaultWalletEvery) })
	}
	// E-mails en file : livrés par le worker (SMTP, reprises espacées).
	stopMail, err := a.startMailWorker()
	if err != nil {
		return fmt.Errorf("mail worker: %w", err)
	}
	defer stopMail()

	// Santé de chaque cluster du registre (alertes ClusterDown / ClusterDegraded).
	a.startClusterHealth(ctx)
	// IP de sortie : celles des projets supprimés reviennent dans la réserve.
	a.startEgressSweep(ctx)

	// VPS : relais de l'outbox vers les workflows du mode compute.
	a.startComputeRelay(ctx)
	a.startComputeRenewals(ctx)
	// Réseau privé des VPS : passerelles alignées sur la base.
	a.startVPCReconcile(ctx)

	// Domaines revendus : commandes à passer et à suivre, renouvellements.
	if sales := a.domainSalesService(); sales != nil && sales.Registrar != nil {
		a.goWith(ctx, "domain-sales", func(ctx context.Context) { sales.Run(ctx, domainsalesSvc.DefaultEvery) })
	}

	// Usage réel des disques : alertes à 80 et 100 %, arrêt du conteneur à 120 %.
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		disks := a.diskUsageService()
		a.goWith(ctx, "disk-usage", func(ctx context.Context) { disks.Run(ctx, diskusageSvc.DefaultEvery) })
	}

	// Sauvegarde nocturne des services qui en ont (MariaDB).
	if b := a.addonBackupService(); b.Backups != nil {
		a.goWith(ctx, "addon-backups", func(ctx context.Context) { b.RunBackups(ctx, addonsSvc.BackupEvery) })
	}

	// « Me prévenir » : e-mail quand un service annoncé ouvre (ADR 0013).
	if n := a.alertNotifier(); n != nil {
		waitlist := &addonsSvc.Service{Waitlists: &addonsSvc.GormWaitlist{DB: a.DB}, Announcer: n,
			Logger: a.Logger.With("component", "addon-waitlist")}
		a.goWith(ctx, "addon-waitlist", func(ctx context.Context) { waitlist.RunWaitlistNotifier(ctx, addonsSvc.WaitlistEvery) })
	}

	// Une panne du worker doit sortir en erreur : sinon le code 0 ne déclenche
	// aucun redémarrage selon la politique du conteneur, et rien ne la signale.
	var workerErr error
	select {
	case <-ctx.Done():
		a.Logger.Infow("worker shutdown signal received")
	case workerErr = <-errCh:
		if workerErr != nil {
			a.Logger.Errorw("temporal worker failed", "error", workerErr)
		}
	}

	processor.Shutdown()
	if healthSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.Config.Server.ShutdownTimeout)
		defer cancel()
		_ = healthSrv.Shutdown(shutdownCtx)
	}
	if workerErr != nil {
		return fmt.Errorf("temporal worker: %w", workerErr)
	}
	a.Logger.Infow("worker stopped gracefully")
	return nil
}

func (a *App) startHealthServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"UP","role":"worker"}`))
	})
	mux.HandleFunc(workerReadyPath, func(w http.ResponseWriter, r *http.Request) {
		status, body := runReadiness(r.Context(), a.readinessChecks(), a.Logger)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	addr := fmt.Sprintf(":%d", a.Config.Server.HTTPPort)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		a.Logger.Infow("health server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.Logger.Warnw("health server error", "error", err)
		}
	}()
	return srv
}

func (a *App) shutdownInfra() {
	// Tâches de fond d'abord : elles utilisent encore la base, Redis, Temporal.
	a.stopBackground()
	if a.UsageAccumulator != nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.UsageAccumulator.Stop(flushCtx)
		cancel()
	}
	if a.AsyncPool != nil {
		a.AsyncPool.Shutdown(10 * time.Second)
	}
	// Plus personne ne s'en sert : clients et caches fermés.
	if a.Redis != nil {
		if err := a.Redis.Close(); err != nil && a.Logger != nil {
			a.Logger.Warnw("redis close", "error", err)
		}
	}
	if c, ok := a.AuthCache.(interface{ Close() }); ok {
		c.Close()
	}
	if a.DB != nil {
		if sqlDB, err := a.DB.DB(); err == nil {
			if err := sqlDB.Close(); err != nil && a.Logger != nil {
				a.Logger.Warnw("database close", "error", err)
			}
		}
	}
	if a.Observability.TracerShutdown != nil {
		a.Observability.TracerShutdown()
	}
	if a.Temporal.Client != nil {
		a.Temporal.Client.Close()
	}

	if a.Observability.TemporalMetricsCloser != nil {
		_ = a.Observability.TemporalMetricsCloser.Close()
	}
	if a.Logger != nil {
		_ = a.Logger.Sync()
	}
}
