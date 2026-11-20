package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/loopfz/gadgeto/tonic"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/addons"
	addonsdomain "github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
	"github.com/thekrauss/kubemanager/internal/modules/diskresize"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	wdomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// newDiskResizeController : agrandissement des disques par copie. Sans
// cluster, les lectures restent possibles et les demandes sont refusées.
func (a *App) newDiskResizeController() *diskresize.Controller {
	s := &diskresize.Service{Store: &diskresize.GormStore{DB: a.DB}, Logger: a.Logger.With("component", "disk-resize"),
		Spawner: a.spawner()}
	if a.Services != nil && a.Services.Workload != nil {
		s.Workloads = &resizeWorkloads{app: a}
	}
	if a.K8sProvider != nil && a.K8sProvider.Client != nil {
		s.Claims = &diskresize.KubeClaims{Kube: a.K8sProvider.Client, Image: "busybox:1.37.0", Timeout: time.Hour}
	}
	return &diskresize.Controller{Service: s, Addons: &resizeAddons{db: a.DB}}
}

// resizeWorkloads : conteneurs vus par l'agrandissement.
type resizeWorkloads struct{ app *App }

// Utilisateur et groupe par défaut du chart (values.yaml).
const chartUID, chartGID = 1000, 2000

func (r *resizeWorkloads) Get(ctx context.Context, id uuid.UUID) (*diskresize.Workload, error) {
	w, err := r.app.Repos.Workload.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var spec wdomain.CreateWorkloadRequest
	if len(w.Spec) > 0 {
		_ = json.Unmarshal(w.Spec, &spec)
	}
	out := &diskresize.Workload{ID: w.ID, ProjectID: w.ProjectID, Name: w.Name, Namespace: w.Namespace,
		Persistent: w.PersistenceEnabled, StorageSize: w.StorageSize, Status: w.Status, Managed: w.ManagedBy != "",
		UID: spec.RunAsUser, GID: spec.RunAsGroup}
	if out.UID <= 0 {
		out.UID = chartUID
	}
	if out.GID <= 0 {
		out.GID = chartGID
	}
	return out, nil
}

func (r *resizeWorkloads) Stop(ctx context.Context, id uuid.UUID) error {
	return r.app.Services.Workload.Stop(ctx, id)
}

func (r *resizeWorkloads) Start(ctx context.Context, id uuid.UUID) error {
	return r.app.Services.Workload.Start(ctx, id)
}

// ApplySize : nouvelle taille enregistrée et redéploiement (le chart demande
// alors la taille du disque recréé : rien à agrandir). Le disque d'un
// service est aussi noté sur le service.
func (r *resizeWorkloads) ApplySize(ctx context.Context, id uuid.UUID, size string) error {
	if err := r.app.Services.Workload.Update(ctx, id.String(), wdomain.UpdateWorkloadRequest{StorageSize: size}); err != nil {
		return err
	}
	return r.app.DB.WithContext(ctx).Model(&addonsdomain.Addon{}).Where("workload_id = ?", id).Update("disk", size).Error
}

func (r *resizeWorkloads) Storage(ctx context.Context, project uuid.UUID) (int64, int64, error) {
	_, _, used, err := r.app.Repos.Workload.GetTotalUsageByProject(ctx, project)
	if err != nil {
		return 0, 0, err
	}
	p, err := r.app.Repos.Project.GetProjectByID(ctx, project.String())
	if err != nil {
		return 0, 0, err
	}
	return used, utils.ParseStorageToBytes(p.StorageLimit), nil
}

// resizeAddons : service → conteneur porteur et disques de son modèle.
type resizeAddons struct{ db *gorm.DB }

func (r *resizeAddons) Carrier(ctx context.Context, project, id uuid.UUID) (uuid.UUID, []string, error) {
	a, err := (&addons.GormRepository{DB: r.db}).Get(ctx, project, id)
	if errors.Is(err, addonsdomain.ErrNotFound) {
		return uuid.Nil, nil, diskresize.ErrNotFound
	}
	if err != nil {
		return uuid.Nil, nil, err
	}
	t, ok := addonsdomain.TemplateByID(a.Template)
	if !ok {
		return uuid.Nil, nil, diskresize.ErrNotFound
	}
	return a.WorkloadID, t.Disks, nil
}

// addDiskResizeRoutes : /workloads/:id/disk et /projects/:id/addons/:addonID/disk.
func addDiskResizeRoutes(app *App) {
	r := app.Controllers.DiskResize
	if r == nil {
		return
	}
	g := newGuards(app)
	workload := workloadResource(app.Repos.Workload)
	WorkloadGroup.AddRoute("/:id/disk", http.MethodGet, "Agrandissement du disque : dernière opération et tailles proposées",
		g.Perm(iam.WorkloadsGet, workload), tonic.Handler(r.GetWorkload, http.StatusOK))
	WorkloadGroup.AddRoute("/:id/disk", http.MethodPost, "Agrandir le disque d'un conteneur (arrêt, copie vérifiée, relance ; en arrière-plan)",
		g.Perm(iam.WorkloadsUpdate, workload), tonic.Handler(r.ResizeWorkload, http.StatusAccepted)).
		ForCopilot(CopilotTool{Name: "resize_workload_disk", Write: true, Risk: tools.RiskMedium,
			Hint: "Grows the disk of a container that has one: the container is stopped during the copy (a few minutes), its data is copied and checked, then it starts again. Sizes only grow. Follow progress with GET on the same path."})
	if app.Controllers.Addons == nil {
		return
	}
	addon := security.ProjectChild("id", "addons", "addonID")
	grp := RootGroup.NewGroup("/projects/:id/addons", "Services").
		Named("Services", "Services additionnels (Redis via Valkey, MariaDB, Meilisearch) : installés et surveillés par betorun, joignables seulement depuis les conteneurs du projet.")
	grp.AddRoute("/:addonID/disk", http.MethodGet, "Agrandissement du disque d'un service : dernière opération et tailles proposées",
		g.Perm(iam.AddonsGet, addon), tonic.Handler(r.GetAddon, http.StatusOK))
	grp.AddRoute("/:addonID/disk", http.MethodPost, "Agrandir le disque d'un service (arrêt, copie vérifiée, relance ; en arrière-plan)",
		g.Perm(iam.AddonsUpdate, addon), tonic.Handler(r.ResizeAddon, http.StatusAccepted))
}
