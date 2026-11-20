package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/addons"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// newAddonsController : services additionnels (ADR 0013). Sans module des
// conteneurs, le contrôleur existe mais répond « indisponible ».
func (a *App) newAddonsController() *addons.Controller {
	s := &addons.Service{Repo: &addons.GormRepository{DB: a.DB}, Projects: a.Repos.Project, Logger: a.Logger,
		Waitlists: &addons.GormWaitlist{DB: a.DB}, Spawner: a.spawner()}
	if a.Services != nil && a.Services.Workload != nil && a.Repos.Workload != nil && a.Variables != nil {
		s.Workloads = &addonWorkloads{repo: a.Repos.Workload, svc: a.Services.Workload, vars: a.Variables}
	}
	s.Backups = a.addonBackups()
	ctl := addons.NewController(s)
	if a.IAM != nil { // pas d'interface non nulle autour d'un *Engine nil
		ctl.IAM = a.IAM
	}
	return ctl
}

// addonBackups : sauvegardes des services (MariaDB). Il faut le cluster, le
// stockage d'objets, l'API joignable des pods et le secret de signature des
// builds ; sinon les sauvegardes sont indisponibles.
func (a *App) addonBackups() *addons.BackupConfig {
	c := a.Config.Builds.WithDefaults()
	if a.K8sProvider == nil || a.K8sProvider.Client == nil || a.ObjectStore == nil || c.Secret == "" || c.PublicAPIURL == "" {
		return nil
	}
	fetch := a.Config.Databases.ImportFetchImage
	if fetch == "" {
		fetch = defaultImportFetchImage
	}
	return &addons.BackupConfig{
		Store: &addons.GormRepository{DB: a.DB}, Objects: a.ObjectStore,
		Signer: registry.Credentials{Secret: []byte(c.Secret)}, Runner: &addons.KubeBackups{Kube: a.K8sProvider.Client},
		PublicAPIURL: c.PublicAPIURL, FetchImage: fetch, EgressAllow: c.EgressAllowCIDRs,
	}
}

// addonBackupService : passage nocturne (worker).
func (a *App) addonBackupService() *addons.Service {
	s := &addons.Service{Repo: &addons.GormRepository{DB: a.DB}, Projects: a.Repos.Project,
		Logger: a.Logger.With("component", "addon-backups"), Backups: a.addonBackups(), Spawner: a.spawner()}
	if a.Services != nil && a.Services.Workload != nil && a.Repos.Workload != nil && a.Variables != nil {
		s.Workloads = &addonWorkloads{repo: a.Repos.Workload, svc: a.Services.Workload, vars: a.Variables}
	}
	return s
}

// addonWorkloads : les conteneurs vus par le module des services. Le
// conteneur porteur passe par le même chemin que la console (quotas,
// variables chiffrées, saga de déploiement).
type addonWorkloads struct {
	repo workloadsRepo.WorkloadRepository
	svc  workloadsSvc.IWorkloadService
	vars workloadsRepo.VariableStore
}

func (w *addonWorkloads) Deploy(ctx context.Context, c addons.Carrier) (uuid.UUID, error) {
	req := workloadsDomain.CreateWorkloadRequest{
		ProjectID: c.ProjectID.String(), Name: c.Name, Image: c.Image, Replicas: 1,
		Kind: workloadsDomain.KindService, Command: c.Command, HealthCommand: c.Health,
		CPURequest: c.CPURequest, CPULimit: c.CPULimit, MemoryRequest: c.Memory, MemoryLimit: c.Memory,
		TargetPort: c.Port, ServiceType: "ClusterIP",
		PersistenceEnabled: c.Disk != "", StorageSize: c.Disk, MountPath: c.DiskPath,
		// Utilisateur de l'image, et groupe du disque (fsGroup) pour qu'il y écrive.
		RunAsUser: c.RunAsUser, RunAsGroup: c.RunAsUser,
		EnvVars:    c.Env,
		SecretData: c.Secrets,
		ManagedBy:  c.ManagedBy,
	}
	wl, err := w.svc.Deploy(ctx, &workloadsSvc.WorkloadServiceRequest{CreateWorkloadRequest: req, Caller: iam.Principal(c.Caller)})
	if err != nil {
		return uuid.Nil, err
	}
	return wl.ID, nil
}

func (w *addonWorkloads) Delete(ctx context.Context, id uuid.UUID) error {
	return w.svc.Delete(ctx, id)
}

func (w *addonWorkloads) SetImage(ctx context.Context, id uuid.UUID, image string) error {
	return w.svc.Update(ctx, id.String(), workloadsDomain.UpdateWorkloadRequest{Image: image})
}

func (w *addonWorkloads) States(ctx context.Context, projectID uuid.UUID) (map[uuid.UUID]addons.CarrierState, error) {
	list, err := w.svc.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]addons.CarrierState, len(list))
	for _, wl := range list {
		out[wl.ID] = addons.CarrierState{Status: wl.Status, Phase: wl.CurrentPhase}
	}
	return out, nil
}

func (w *addonWorkloads) NameTaken(ctx context.Context, projectID uuid.UUID, name string) (bool, error) {
	list, err := w.svc.List(ctx, projectID)
	if err != nil {
		return false, err
	}
	for _, wl := range list {
		if wl.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (w *addonWorkloads) ProjectOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return wl.ProjectID, nil
}

func (w *addonWorkloads) NameOf(ctx context.Context, id uuid.UUID) (string, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return wl.Name, nil
}

// Secret : valeur en clair d'une variable (lue côté serveur, jamais renvoyée).
func (w *addonWorkloads) Secret(ctx context.Context, id uuid.UUID, name string) (string, error) {
	values, err := w.vars.Values(ctx, id)
	if err != nil {
		return "", err
	}
	return values[name], nil
}

func (w *addonWorkloads) WaitFor(ctx context.Context, id uuid.UUID, target string) error {
	return w.svc.AddWaitFor(ctx, id, target)
}

// SetSecrets : variables secrètes, avec redéploiement si une valeur change.
func (w *addonWorkloads) SetSecrets(ctx context.Context, id uuid.UUID, values map[string]string) (bool, error) {
	res, err := w.svc.PatchVariables(ctx, id, workloadsDomain.PatchVariablesRequest{Secrets: values})
	if err != nil {
		return false, err
	}
	return res.Changed, nil
}

func addAddonRoutes(app *App) {
	r := app.Controllers.Addons
	if r == nil {
		return
	}
	g := newGuards(app)
	grp := RootGroup.NewGroup("/projects/:id/addons", "Services").
		Named("Services", "Services additionnels (Redis via Valkey, MariaDB, Meilisearch) : installés et surveillés par betorun, joignables seulement depuis les conteneurs du projet.")
	project := security.ProjectParam("id")
	addon := security.ProjectChild("id", "addons", "addonID")

	grp.AddRoute("/templates", http.MethodGet, "Catalogue des services : versions, usages, tailles et disques proposés",
		g.Perm(iam.AddonsList, project), tonic.Handler(r.Templates, http.StatusOK))
	grp.AddRoute("", http.MethodGet, "Lister les services du projet (avec le plafond)", g.Perm(iam.AddonsList, project),
		tonic.Handler(r.List, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "list_addons"})
	grp.AddRoute("", http.MethodPost, "Ajouter un service (Redis via Valkey, MariaDB, Meilisearch) ; mot de passe généré, jamais renvoyé",
		g.Perm(iam.AddonsCreate, project), tonic.Handler(r.Create, http.StatusCreated)).
		ForCopilot(CopilotTool{Name: "create_addon", Write: true, Risk: tools.RiskLow, Only: []string{"template", "name", "version", "mode", "size", "disk"},
			Hint: "Adds a catalog service, reachable only from the project's containers: \"redis\" = Valkey (Redis-compatible; mode persistent, the default, or cache), \"mariadb\" (MySQL-compatible, database and user app) or \"meilisearch\" (search engine). Then propose attach_addon to give a container its connection variable."})
	grp.AddRoute("/:addonID", http.MethodGet, "Détails et état d'un service (jamais le mot de passe)", g.Perm(iam.AddonsGet, addon),
		tonic.Handler(r.Get, http.StatusOK))
	grp.AddRoute("/:addonID/upgrade", http.MethodPost, "Passer à une version plus récente (le service redémarre ; jamais de retour en arrière)",
		g.Perm(iam.AddonsUpdate, addon), tonic.Handler(r.Upgrade, http.StatusOK))
	grp.AddRoute("/:addonID/attach", http.MethodPost, "Brancher le service sur un conteneur (adresse posée en variable secrète, conteneur redéployé)",
		g.Perm(iam.AddonsConnect, addon), tonic.Handler(r.Attach, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "attach_addon", Write: true, Risk: tools.RiskMedium,
			Hint: "Sets the connection URL (password included) as a SECRET variable of the container (REDIS_URL by default for redis, DATABASE_URL for mariadb; meilisearch: MEILISEARCH_URL plus the key in MEILISEARCH_KEY) and redeploys it. The secrets never go through you."})
	// « Me prévenir » : personnel, hors projet.
	wl := RootGroup.NewGroup("/addons/waitlist", "Services").
		Named("Services", "Services additionnels (Redis via Valkey, MariaDB, Meilisearch) : installés et surveillés par betorun, joignables seulement depuis les conteneurs du projet.")
	wl.AddRoute("", http.MethodGet, "Services annoncés pour lesquels je serai prévenu par e-mail à l'ouverture",
		Authenticated(), tonic.Handler(r.Waitlist, http.StatusOK))
	wl.AddRoute("/:template", http.MethodPut, "Être prévenu par e-mail quand un service annoncé ouvre (un seul e-mail)",
		Authenticated(), tonic.Handler(r.JoinWaitlist, http.StatusOK))
	wl.AddRoute("/:template", http.MethodDelete, "Ne plus être prévenu de l'ouverture d'un service annoncé",
		Authenticated(), tonic.Handler(r.LeaveWaitlist, http.StatusOK))

	grp.AddRoute("/:addonID", http.MethodDelete, "Supprimer un service et son disque (les conteneurs branchés gardent leur variable)",
		g.Perm(iam.AddonsDelete, addon), tonic.Handler(r.Delete, http.StatusOK))

	// Sauvegardes (services qui en ont : MariaDB).
	grp.AddRoute("/:addonID/backups", http.MethodGet, "Sauvegardes d'un service (une chaque nuit, les 7 dernières gardées)",
		g.Perm(iam.AddonsGet, addon), tonic.Handler(r.ListBackups, http.StatusOK))
	grp.AddRoute("/:addonID/backups", http.MethodPost, "Sauvegarder un service tout de suite (la copie se fait en arrière-plan)",
		g.Perm(iam.AddonsUpdate, addon), tonic.Handler(r.CreateBackup, http.StatusAccepted))
	grp.AddRoute("/:addonID/backups/:backupID/restore", http.MethodPost, "Restaurer une sauvegarde (copie de l'état actuel d'abord ; les données actuelles sont remplacées)",
		g.Perm(iam.AddonsRestore, addon), tonic.Handler(r.RestoreBackup, http.StatusOK))
	// Interne, sans session : les Jobs envoient et lisent les copies par URL signée.
	transfers := RootGroup.NewGroup("/addons/backups", "Copie d'un service, pour son Job").Internal()
	transfers.AddRoute("/:taskID/upload", http.MethodPut, "Copie d'un service envoyée par son Job (URL signée)", Public(), wrapRawHandler(r.UploadBackup))
	transfers.AddRoute("/:taskID/download", http.MethodGet, "Sauvegarde lue par le Job de restauration (URL signée)", Public(), wrapRawHandler(r.DownloadBackup))
}
