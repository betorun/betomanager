package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	"github.com/thekrauss/kubemanager/internal/modules/databases"
	databasesDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	databasesRepo "github.com/thekrauss/kubemanager/internal/modules/databases/repository"
	databasesSvc "github.com/thekrauss/kubemanager/internal/modules/databases/service"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// newDatabasesController : bases gérées. Sans accès au cluster, le contrôleur
// existe mais répond « indisponible » (Clusters nil).
func (a *App) newDatabasesController() *databases.Controller {
	repo := &databasesRepo.GormRepository{DB: a.DB}
	s := &databasesSvc.Service{Repo: repo, Projects: a.Repos.Project, Attachments: repo, Logger: a.Logger}
	if a.Services != nil && a.Services.Workload != nil && a.Repos.Workload != nil {
		s.Workloads = &databaseWorkloads{repo: a.Repos.Workload, svc: a.Services.Workload}
	}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil && a.K8sProvider.Config != nil {
		// Chaque base sur le cluster de son projet (lot 2b).
		cnpg := &databasesRepo.CNPG{Clients: a.cnpgClients(), StorageClass: a.Config.Databases.StorageClass,
			StorageFor: a.storageFor()}
		if target, err := databasesRepo.BackupTargetFrom(a.Config.Databases.Backup); err != nil {
			a.Logger.Errorw("databases: backups disabled (configuration)", "error", err)
		} else if target != nil {
			cnpg.Backup = target
		} else {
			a.Logger.Warnw("databases: no backup storage configured (databases.backup.*): new databases will have NO backups")
		}
		s.Clusters = cnpg
		s.Shell = cnpg
		s.Snapshots = repo
		s.Archives = repo
		s.Imports = a.databaseImports(cnpg)
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
func (a *App) databaseImports(importer databasesSvc.Importer) *databasesSvc.ImportConfig {
	c := a.Config.Builds.WithDefaults()
	if a.ObjectStore == nil || c.Secret == "" || c.PublicAPIURL == "" {
		a.Logger.Warnw("databases: dump import disabled (object store, builds.secret or builds.public_api_url missing)")
		return nil
	}
	fetch := a.Config.Databases.ImportFetchImage
	if fetch == "" {
		fetch = defaultImportFetchImage
	}
	return &databasesSvc.ImportConfig{
		Store: &databasesRepo.GormRepository{DB: a.DB}, Dumps: a.ObjectStore,
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

// NameOf : nom du conteneur (tracé avec le branchement).
func (w *databaseWorkloads) NameOf(ctx context.Context, id uuid.UUID) (string, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return wl.Name, nil
}

func (w *databaseWorkloads) ProjectOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return wl.ProjectID, nil
}

func (w *databaseWorkloads) WaitFor(ctx context.Context, id uuid.UUID, target string) error {
	return w.svc.AddWaitFor(ctx, id, target)
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
	dbs.AddRoute("/:databaseID/stop", http.MethodPost, "Arrêter une base (ses données sont gardées ; les conteneurs branchés perdent la connexion)",
		g.Perm(iam.DatabasesStop, db), tonic.Handler(r.Stop, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "stop_database", Write: true, Risk: tools.RiskMedium,
			Hint: "Stops the database instance; its disk and data are kept. Containers using it lose their connection until start_database."})
	dbs.AddRoute("/:databaseID/start", http.MethodPost, "Relancer une base arrêtée (prête en une minute environ)",
		g.Perm(iam.DatabasesStop, db), tonic.Handler(r.Start, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "start_database", Write: true, Risk: tools.RiskLow,
			Hint: "Starts a stopped database again on the same disk; ready in about a minute."})
	dbs.AddRoute("/:databaseID", http.MethodDelete, "Supprimer une base (ses données sont perdues)", g.Perm(iam.DatabasesDelete, db),
		tonic.Handler(r.Delete, http.StatusNoContent))

	// Import d'un dump (multipart : dump, clean) : hors tonic, lu en flux.
	dbs.AddRoute("/:databaseID/import", http.MethodPost, "Importer un dump pg_dump -Fc dans la base (restauré par un Job du projet)",
		g.Perm(iam.DatabasesImport, db), wrapRawHandler(r.Import)).
		Documented(RawDoc{Input: databasesDomain.ImportUploadPath{}, Status: http.StatusAccepted, Produces: MIMEJSON, Output: databasesDomain.DatabaseImport{},
			Multipart: []FormField{
				{Name: databasesDomain.DumpFormField, Description: "Dump au format personnalisé de pg_dump (-Fc)", File: true, Required: true},
				{Name: "clean", Description: "true : supprimer d'abord les objets existants du dump"},
				{Name: "reset", Description: "true : vider d'abord la base (copie de sécurité gardée 24 h)"},
			}})
	// Inspection : le début d'un dump comparé à la base (rien n'est écrit).
	dbs.AddRoute("/:databaseID/import/inspect", http.MethodPost, "Comparer un dump à la base avant de l'importer (schémas et tables ; seul le début du fichier est lu)",
		g.Perm(iam.DatabasesGet, db), wrapRawHandler(r.Inspect)).
		Documented(RawDoc{Input: databasesDomain.ImportUploadPath{}, Status: http.StatusOK, Produces: MIMEJSON, Output: databasesDomain.InspectResult{},
			Multipart: []FormField{
				{Name: databasesDomain.DumpFormField, Description: "Début du dump pg_dump -Fc (16 Mio suffisent)", File: true, Required: true},
			}})
	dbs.AddRoute("/:databaseID/contents", http.MethodGet, "Contenu d'une base : taille, et tables par schéma (lu dans la base)",
		g.Perm(iam.DatabasesGet, db), tonic.Handler(r.Contents, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_database_contents"})
	dbs.AddRoute("/:databaseID/reset", http.MethodPost, "Vider la base (copie de sécurité gardée 24 h ; la base et ses branchements restent)",
		g.Perm(iam.DatabasesReset, db), tonic.Handler(r.Reset, http.StatusOK))
	dbs.AddRoute("/:databaseID/reset/undo", http.MethodPost, "Restaurer la copie prise avant la remise à zéro (suivie comme un import)",
		g.Perm(iam.DatabasesReset, db), tonic.Handler(r.UndoReset, http.StatusAccepted))
	dbs.AddRoute("/:databaseID/imports", http.MethodGet, "Imports récents d'une base", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.ListImports, http.StatusOK))
	dbs.AddRoute("/:databaseID/imports/:importID", http.MethodGet, "État d'un import (journal de pg_restore en cas d'échec)", g.Perm(iam.DatabasesGet, db),
		tonic.Handler(r.GetImport, http.StatusOK))

	// Interne, sans session : le Job d'import lit le dump par une URL signée.
	internal := RootGroup.NewGroup("/databases/imports", "Dump d'un import pour son Job").Internal()
	internal.AddRoute("/:importID/dump", http.MethodGet, "Dump d'un import (URL signée)", Public(), wrapRawHandler(r.Dump))
	// Interne, sans session : le Job de remise à zéro envoie la copie de sécurité.
	snaps := RootGroup.NewGroup("/databases/snapshots", "Copie de sécurité d'une base, envoyée par son Job").Internal()
	snaps.AddRoute("/:taskID/upload", http.MethodPut, "Copie de sécurité avant remise à zéro (URL signée)", Public(), wrapRawHandler(r.UploadSnapshot))
}
