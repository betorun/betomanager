package router

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	projectRepos "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	"github.com/thekrauss/kubemanager/internal/modules/storages3"
	storageActivities "github.com/thekrauss/kubemanager/internal/modules/storages3/activities"
	storageDomain "github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
	storageRepo "github.com/thekrauss/kubemanager/internal/modules/storages3/repository"
	storageSvc "github.com/thekrauss/kubemanager/internal/modules/storages3/service"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	workloadsRepo "github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// newStorageS3Controller : stockage objet des clients. Sans configuration
// storage_s3, le contrôleur existe mais répond « indisponible ».
func (a *App) newStorageS3Controller() *storages3.Controller {
	s := &storageSvc.Service{Repo: &storageRepo.GormRepository{DB: a.DB}, Logger: a.Logger}
	if a.Repos != nil && a.Repos.Project != nil {
		s.Projects = &storageProjects{repo: a.Repos.Project}
	}
	if a.Services != nil && a.Services.Workload != nil && a.Repos.Workload != nil {
		s.Workloads = &storageWorkloads{repo: a.Repos.Workload, svc: a.Services.Workload}
	}
	a.withStorageBackend(s)
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		s.Alerts = n
	}
	ctl := storages3.NewController(s)
	ctl.WriteError = writeClassifiedError
	if a.IAM != nil {
		ctl.IAM = a.IAM
	}
	return ctl
}

// withStorageBackend : S3 de la plateforme, s'il est configuré.
func (a *App) withStorageBackend(s *storageSvc.Service) {
	c := a.Config.StorageS3
	if c.Endpoint == "" {
		a.Logger.Infow("storage_s3: not configured (storage_s3.endpoint): object storage disabled")
		return
	}
	backend, err := storageRepo.NewS3(storageRepo.S3Config{Endpoint: c.Endpoint, UseSSL: c.UseSSL, Region: c.Region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, PublicEndpoint: c.PublicEndpoint})
	if err != nil {
		a.Logger.Errorw("storage_s3: disabled (configuration)", "error", err)
		return
	}
	s.Backend = backend
	s.Endpoint = c.PublicEndpoint
	s.Region = c.Region
	if s.Region == "" {
		s.Region = "us-east-1"
	}
}

// storagePurger : effacement des buckets d'un projet (worker Temporal, et
// délai de grâce côté API) ; nil sans stockage configuré.
func (a *App) storagePurger() storageActivities.ProjectPurger {
	s := &storageSvc.Service{Repo: &storageRepo.GormRepository{DB: a.DB}, Logger: a.Logger}
	a.withStorageBackend(s)
	if s.Backend == nil {
		return nil
	}
	return s
}

// startStorageUsage : espace utilisé et quota des buckets, toutes les 15 min.
func (a *App) startStorageUsage(ctx context.Context) {
	if a.Controllers == nil || a.Controllers.StorageS3 == nil {
		return
	}
	a.Controllers.StorageS3.Service.StartUsage(ctx)
}

// storageProjects : projet prêt (pas en création ni en suppression).
type storageProjects struct {
	repo projectRepos.ProjectRepository
}

func (p *storageProjects) ProjectReady(ctx context.Context, id uuid.UUID) error {
	pr, err := p.repo.GetProjectByID(ctx, id.String())
	if err != nil {
		return err
	}
	if pr.Status != utils.ProjectStatusReady {
		return storageSvc.ErrProjectNotReady
	}
	return nil
}

// storageWorkloads : les conteneurs vus par le module de stockage.
type storageWorkloads struct {
	repo workloadsRepo.WorkloadRepository
	svc  workloadsSvc.IWorkloadService
}

func (w *storageWorkloads) ProjectOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return wl.ProjectID, nil
}

func (w *storageWorkloads) NameOf(ctx context.Context, id uuid.UUID) (string, error) {
	wl, err := w.repo.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return wl.Name, nil
}

// SetVariables : même chemin que la console (chiffrement, journal, saga).
func (w *storageWorkloads) SetVariables(ctx context.Context, id uuid.UUID, env, secrets map[string]string, unset []string) (bool, error) {
	res, err := w.svc.PatchVariables(ctx, id, workloadsDomain.PatchVariablesRequest{Env: env, Secrets: secrets, Unset: unset})
	if err != nil {
		return false, err
	}
	return res.Changed, nil
}

func addStorageS3Routes(app *App) {
	r := app.Controllers.StorageS3
	if r == nil {
		return
	}
	g := newGuards(app)
	grp := RootGroup.NewGroup("/projects/:id/storage/buckets", "Stockage objet").
		Named("Stockage objet", "Buckets S3 du projet : fichiers des applications, joignables avec les bibliothèques S3 habituelles.")
	project := security.ProjectParam("id")
	bucket := security.ProjectChild("id", "buckets", "bucketID")

	grp.AddRoute("", http.MethodGet, "Lister les buckets du projet (espace utilisé, clés)", g.Perm(iam.StorageBucketsList, project),
		tonic.Handler(r.List, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "list_buckets"})
	grp.AddRoute("", http.MethodPost, "Créer un bucket (nom S3 généré, quota en Go)", g.Perm(iam.StorageBucketsCreate, project),
		tonic.Handler(r.Create, http.StatusCreated)).
		ForCopilot(CopilotTool{Name: "create_bucket", Write: true, Risk: tools.RiskLow, Only: []string{"name", "quota_gb"},
			Hint: "Object storage (S3 API) for the project's files, billed per GB used (storage_gb_monthly in get_price_catalog; 0: free). Then propose attach_bucket to give a container its S3_* variables."})
	grp.AddRoute("/:bucketID", http.MethodGet, "Détails d'un bucket (jamais de secret)", g.Perm(iam.StorageBucketsGet, bucket),
		tonic.Handler(r.Get, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "get_bucket"})
	grp.AddRoute("/:bucketID", http.MethodPatch, "Changer le quota d'un bucket", g.Perm(iam.StorageBucketsUpdate, bucket),
		tonic.Handler(r.Update, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "set_bucket_quota", Write: true, Risk: tools.RiskLow,
			Hint: "Over its quota a bucket becomes read-only until space is freed or the quota raised."})
	grp.AddRoute("/:bucketID/attach", http.MethodPost, "Brancher le bucket sur un conteneur (clé propre au conteneur, variables posées, conteneur redéployé)",
		g.Perm(iam.StorageBucketsConnect, bucket), tonic.Handler(r.Attach, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "attach_bucket", Write: true, Risk: tools.RiskMedium,
			Hint: "Creates a key for this container only and sets S3_ENDPOINT, S3_REGION, S3_BUCKET (plain) and S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY (secrets), then redeploys it. The secret never goes through you."})
	grp.AddRoute("/:bucketID/keys", http.MethodPost, "Créer une clé d'accès (son secret n'est montré qu'une fois)",
		g.Perm(iam.StorageBucketsConnect, bucket), tonic.Handler(r.CreateKey, http.StatusCreated))
	grp.AddRoute("/:bucketID/keys/:keyID", http.MethodDelete, "Révoquer une clé (branchement : variables retirées du conteneur)",
		g.Perm(iam.StorageBucketsConnect, bucket), tonic.Handler(r.DeleteKey, http.StatusNoContent))
	grp.AddRoute("/:bucketID/keys/:keyID/rotate", http.MethodPost, "Faire tourner le secret d'une clé (branchement : posé dans le conteneur, redéployé ; clé manuelle : montré une fois)",
		g.Perm(iam.StorageBucketsConnect, bucket), tonic.Handler(r.RotateKey, http.StatusOK))
	grp.AddRoute("/:bucketID/public", http.MethodPut, "Ouvrir ou fermer la lecture publique (lire un fichier par son adresse ; lister reste refusé)",
		g.Perm(iam.StorageBucketsSetPublic, bucket), tonic.Handler(r.SetPublic, http.StatusOK))
	grp.AddRoute("/:bucketID", http.MethodDelete, "Supprimer un bucket (vide, ou confirmé par son nom : son contenu est effacé)",
		g.Perm(iam.StorageBucketsDelete, bucket), tonic.Handler(r.Delete, http.StatusNoContent))

	// Explorateur de fichiers (console, CLI) : par l'API, jamais de clé.
	grp.AddRoute("/:bucketID/objects", http.MethodGet, "Lister un dossier du bucket (dossiers d'abord, par pages)",
		g.Perm(iam.StorageObjectsList, bucket), tonic.Handler(r.ListObjects, http.StatusOK)).
		ForCopilot(CopilotTool{Name: "list_bucket_files"})
	grp.AddRoute("/:bucketID/objects/download", http.MethodGet, "Télécharger un fichier",
		g.Perm(iam.StorageObjectsGet, bucket).DataRead(), wrapRawHandler(r.Download)).
		Documented(RawDoc{Input: storageDomain.ObjectRequest{}, Produces: "application/octet-stream"})
	grp.AddRoute("/:bucketID/objects", http.MethodPost, "Envoyer un fichier (multipart, champ file, 100 Mo au plus ; nom terminé par / et sans fichier : nouveau dossier)",
		g.Perm(iam.StorageObjectsCreate, bucket), wrapRawHandler(r.Upload)).
		Documented(RawDoc{Input: storageDomain.UploadObjectRequest{}, Status: http.StatusCreated, Produces: MIMEJSON, Output: storageDomain.Object{},
			Multipart: []FormField{{Name: storageDomain.ObjectFormField, Description: "Contenu du fichier", File: true}}})
	grp.AddRoute("/:bucketID/objects/delete", http.MethodPost, "Supprimer des fichiers (un nom terminé par / supprime tout le dossier)",
		g.Perm(iam.StorageObjectsDelete, bucket), tonic.Handler(r.DeleteObjects, http.StatusOK))
	grp.AddRoute("/:bucketID/objects/link", http.MethodPost, "Lien de lecture d'un fichier, valable 1 h à 7 jours, sans compte",
		g.Perm(iam.StorageObjectsShare, bucket), tonic.Handler(r.ShareLink, http.StatusOK))
}

// storageController : contrôleur du stockage, s'il est construit.
func (a *App) storageController() *storages3.Controller {
	if a.Controllers == nil {
		return nil
	}
	return a.Controllers.StorageS3
}
