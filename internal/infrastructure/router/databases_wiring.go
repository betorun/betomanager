package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	"k8s.io/client-go/dynamic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
	databasesDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// newDatabasesController : bases gérées. Sans accès au cluster, le contrôleur
// existe mais répond « indisponible » (Clusters nil).
func (a *App) newDatabasesController() *databases.Controller {
	s := &databases.Service{Repo: &databases.GormRepository{DB: a.DB}, Projects: a.Repos.Project, Logger: a.Logger}
	if a.Services != nil && a.Services.Workload != nil && a.Repos.Workload != nil {
		s.Workloads = &databaseWorkloads{repo: a.Repos.Workload, svc: a.Services.Workload}
	}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil && a.K8sProvider.Config != nil {
		dyn, err := dynamic.NewForConfig(a.K8sProvider.Config)
		if err != nil {
			a.Logger.Errorw("databases disabled: dynamic client", "error", err)
		} else {
			cnpg := &databases.CNPG{Dynamic: dyn, Kube: a.K8sProvider.Client, StorageClass: a.Config.Databases.StorageClass}
			if target, err := databases.BackupTargetFrom(a.Config.Databases.Backup); err != nil {
				a.Logger.Errorw("databases: backups disabled (configuration)", "error", err)
			} else if target != nil {
				cnpg.Backup = target
			} else {
				a.Logger.Warnw("databases: no backup storage configured (databases.backup.*): new databases will have NO backups")
			}
			s.Clusters = cnpg
			s.Imports = a.databaseImports(cnpg)
		}
	}
	ctl := databases.NewController(s)
	if a.IAM != nil { // pas d'interface non nulle autour d'un *Engine nil
		ctl.IAM = a.IAM
	}
	return ctl
}

// defaultImportFetchImage : télécharge le dump dans le Job d'import.
const defaultImportFetchImage = "curlimages/curl:8.11.1"

// databaseImports : import de dumps. Il faut le stockage d'objets (le dump y
// attend le Job), l'API joignable des pods et le secret de signature des
// builds (URL signées) ; sinon la fonction est indisponible.
func (a *App) databaseImports(importer databases.Importer) *databases.ImportConfig {
	c := a.Config.Builds.WithDefaults()
	if a.ObjectStore == nil || c.Secret == "" || c.PublicAPIURL == "" {
		a.Logger.Warnw("databases: dump import disabled (object store, builds.secret or builds.public_api_url missing)")
		return nil
	}
	fetch := a.Config.Databases.ImportFetchImage
	if fetch == "" {
		fetch = defaultImportFetchImage
	}
	return &databases.ImportConfig{
		Store: &databases.GormRepository{DB: a.DB}, Dumps: a.ObjectStore,
		Signer: registry.Credentials{Secret: []byte(c.Secret)}, Importer: importer,
		PublicAPIURL: c.PublicAPIURL, FetchImage: fetch, EgressAllow: c.EgressAllowCIDRs,
		MaxBytes: a.Config.Databases.ImportMaxBytes,
	}
}

// databaseWorkloads : les conteneurs vus par le module des bases.
type databaseWorkloads struct {
	repo workloadsRepo.WorkloadRepository
	svc  workloadsSvc.IWorkloadService
}

func (w *databaseWorkloads) ProjectOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return wl.ProjectID, nil
}

// SetSecret : variable secrète, avec redéploiement si la valeur change
// (même chemin que la console : chiffrement, journal, saga).
func (w *databaseWorkloads) SetSecret(ctx context.Context, id uuid.UUID, name, value string) (bool, error) {
	res, err := w.svc.PatchVariables(ctx, id, workloadsDomain.PatchVariablesRequest{Secrets: map[string]string{name: value}})
	if err != nil {
		return false, err
	}
	return res.Changed, nil
}

func addDatabaseRoutes(app *App) {
	r := app.Controllers.Databases
	if r == nil {
		return
	}
	g := newGuards(app)
	dbs := RootGroup.NewGroup("/projects/:id/databases", "Bases de données").
		Named("Bases de données", "Bases Postgres gérées : une par besoin, joignables seulement depuis les conteneurs du projet.")
	project := security.ProjectParam("id")
	db := security.ProjectChild("id", "databases", "databaseID")

	dbs.AddRoute("", http.MethodGet, "Lister les bases de données du projet", g.Perm(iam.DatabasesList, project),
		tonic.Handler(r.List, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "list_databases"})
	dbs.AddRoute("", http.MethodPost, "Créer une base Postgres (prête en une minute environ)", g.Perm(iam.DatabasesCreate, project),
		tonic.Handler(r.Create, http.StatusCreated)).
		ForCopilot(CopilotTool{Name: "create_database", Write: true, Risk: tools.RiskLow, Only: []string{"name", "storage_size"},
			Hint: "Managed Postgres, reachable only from the project's containers; ready in about a minute. Then propose attach_database to give a container its DATABASE_URL."})
	dbs.AddRoute("/:databaseID", http.MethodGet, "Détails et état d'une base (jamais le mot de passe)", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_database"})
	dbs.AddRoute("/:databaseID/attach", http.MethodPost, "Brancher la base sur un conteneur (URI posée en variable secrète, conteneur redéployé)",
		g.Perm(iam.DatabasesConnect, db), tonic.Handler(r.Attach, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "attach_database", Write: true, Risk: tools.RiskMedium,
			Hint: "Sets the connection URI (password included) as a SECRET variable of the container, DATABASE_URL by default, and redeploys it. The URI never goes through you."})
	// Restauration : nouvelle base née des sauvegardes de :databaseID (create
	// sur le projet ; le contrôleur vérifie aussi get sur la source).
	dbs.AddRoute("/:databaseID/restore", http.MethodPost, "Restaurer dans une nouvelle base (dernier état ou instant donné ; la source n'est pas modifiée)",
		g.Perm(iam.DatabasesCreate, project), tonic.Handler(r.Restore, http.StatusCreated)).
		ForCopilot(CopilotTool{Name: "restore_database", Write: true, Risk: tools.RiskLow,
			Hint: "Creates a NEW database from the backups of this one (latest state, or target_time in RFC 3339). The source is never changed. Then attach_database switches a container to the copy."})
	dbs.AddRoute("/:databaseID", http.MethodDelete, "Supprimer une base (ses données sont perdues)", g.Perm(iam.DatabasesDelete, db),
		tonic.Handler(r.Delete, http.StatusNoContent))

	// Import d'un dump (multipart : dump, clean) : hors tonic, lu en flux.
	dbs.AddRoute("/:databaseID/import", http.MethodPost, "Importer un dump pg_dump -Fc dans la base (restauré par un Job du projet)",
		g.Perm(iam.DatabasesImport, db), wrapRawHandler(r.Import)).
		Documented(RawDoc{Input: databasesDomain.ImportUploadPath{}, Status: http.StatusAccepted, Produces: MIMEJSON, Output: databasesDomain.DatabaseImport{},
			Multipart: []FormField{
				{Name: databasesDomain.DumpFormField, Description: "Dump au format personnalisé de pg_dump (-Fc)", File: true, Required: true},
				{Name: "clean", Description: "true : supprimer d'abord les objets existants du dump"},
			}})
	dbs.AddRoute("/:databaseID/imports", http.MethodGet, "Imports récents d'une base", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.ListImports, http.StatusOK))
	dbs.AddRoute("/:databaseID/imports/:importID", http.MethodGet, "État d'un import (journal de pg_restore en cas d'échec)", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.GetImport, http.StatusOK))

	// Interne, sans session : le Job d'import lit le dump par une URL signée.
	internal := RootGroup.NewGroup("/databases/imports", "Dump d'un import pour son Job").Internal()
	internal.AddRoute("/:importID/dump", http.MethodGet, "Dump d'un import (URL signée)", Public(), wrapRawHandler(r.Dump))
}
