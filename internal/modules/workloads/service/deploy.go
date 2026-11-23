package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	buildsDomain "github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/activities"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	projectRepo "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// RunAsAuthorizer : contrôle « exécuter comme un compte de service »
// (implémenté par *iam.ServiceAccountService).
type RunAsAuthorizer interface {
	AuthorizeRunAs(ctx context.Context, caller iam.Principal, projectID uuid.UUID, ref string) (uuid.UUID, error)
}

// IdentitySettings : identité des workloads (iam.md §7.2). nil : désactivée.
type IdentitySettings struct {
	Authorizer RunAsAuthorizer
	Audience   string // émetteur du STS
	TokenURI   string
}

// ErrWorkloadIdentityDisabled : pas de cluster ou de STS configuré.
var ErrWorkloadIdentityDisabled = errors.New("workload identity is not enabled on this platform")

// ErrWorkloadNameTaken : le nom sert de release Helm dans l'espace du projet ;
// deux conteneurs du même nom se disputeraient la même release.
var ErrWorkloadNameTaken = errors.New("a container with this name already exists in the project")

type WorkloadService struct {
	// Incidents : lever à la lecture un échec que le cluster dément (nil : la
	// seule passe du worker s'en charge).
	Incidents repository.IncidentStore
	// Identity : nil si l'identité des workloads est désactivée.
	Identity    *IdentitySettings
	Distributor temporal.TaskDistributor
	Repo        repository.WorkloadRepository
	ProjectRepo projectRepo.ProjectRepository
	// Kube : clients du cluster du projet de chaque conteneur.
	Kube   k8sprovider.Resolver
	Logger *zap.SugaredLogger
	// Billing : garde de la facturation (nil : rien n'est bloqué). Refuse de
	// démarrer ou de déployer pendant une suspension faute de solde, ou dans
	// un projet arrêté par son budget.
	Billing AccountGuard
	// Registry : registre de la plateforme (nil si builds désactivés).
	Registry *RegistrySettings
	// Variables : variables et secrets chiffrés.
	Variables repository.VariableStore
	// ExternalRegistries : accès du projet à des registres externes (nil :
	// images publiques et registre de la plateforme seulement).
	ExternalRegistries ExternalRegistries
	// Domains, DomainCfg : domaines personnalisés (nil : indisponibles).
	Domains   repository.DomainStore
	DomainCfg *DomainSettings
	// Clusters : réglages du cluster de chaque projet (ingress, point
	// d'entrée, stockage) ; nil : ceux de la configuration.
	Clusters k8sprovider.SettingsResolver
}

// RegistrySettings : registre d'images de la plateforme.
type RegistrySettings struct {
	Host string
}

// checkImage : une image du registre de la plateforme n'est déployable que
// par son projet.
func (s *WorkloadService) checkImage(project uuid.UUID, image string) error {
	if s.Registry == nil || !registry.IsPlatformImage(s.Registry.Host, image) || registry.OwnedBy(s.Registry.Host, image, project) {
		return nil
	}
	return buildsDomain.ErrForeignImage
}

// Interface compliance check — erreur de compil si méthodes manquantes.
var _ IWorkloadService = (*WorkloadService)(nil)

func NewWorkloadService(
	dist temporal.TaskDistributor,
	repo repository.WorkloadRepository,
	pRepo projectRepo.ProjectRepository,
	kube k8sprovider.Resolver,
	log *zap.SugaredLogger,
) *WorkloadService {
	return &WorkloadService{
		Distributor: dist,
		Repo:        repo,
		ProjectRepo: pRepo,
		Kube:        kube,
		Logger:      log.With("service", "WorkloadService"),
	}
}

// AccountGuard : la facturation autorise-t-elle à faire tourner des
// conteneurs dans ce projet ? (implémenté par le module billing)
type AccountGuard interface {
	CanRun(ctx context.Context, project uuid.UUID) error
}

// canRun : nil sans garde.
func (s *WorkloadService) canRun(ctx context.Context, project uuid.UUID) error {
	if s.Billing == nil {
		return nil
	}
	return s.Billing.CanRun(ctx, project)
}

type WorkloadServiceRequest struct {
	domain.CreateWorkloadRequest
	// Caller : auteur de la demande (contrôle actAs du compte de service).
	Caller iam.Principal
}

func (s *WorkloadService) identity(saID uuid.UUID) *activities.WorkloadIdentity {
	return &activities.WorkloadIdentity{ServiceAccount: string(iam.ServiceAccountPrincipal(saID)),
		Audience: s.Identity.Audience, TokenURI: s.Identity.TokenURI}
}

func (s *WorkloadService) Deploy(ctx context.Context, in *WorkloadServiceRequest) (*domain.Workload, error) {
	workload, spec, err := s.create(ctx, in, utils.WorkloadStarting, utils.PhaseHelmChartLoading)
	if err != nil {
		return nil, err
	}
	prof, err := s.profileFor(ctx, workload.ProjectID)
	if err == nil {
		_, err = s.Distributor.ExecuteWorkflow(ctx,
			temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDDeployPrefix + workload.ID.String()},
			workflows.DeployWorkloadWorkflowName, s.deployInput(workload, spec, prof))
	}
	if err != nil {
		// Rien n'est installé : la ligne STARTING garderait le nom et le quota.
		s.compensate(ctx, "workload creation not undone", workload.ID, func(ctx context.Context) error {
			return s.Repo.Delete(ctx, workload.ID)
		})
		return nil, err
	}
	return workload, nil
}

// compensate : annule une écriture dont la suite a échoué, y compris si la
// requête a été interrompue. Erreur journalisée : celle de l'appelant prime.
func (s *WorkloadService) compensate(ctx context.Context, msg string, id uuid.UUID, undo func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	if err := undo(ctx); err != nil {
		s.warn(msg, "workload", id, "error", err)
	}
}

func (s *WorkloadService) warn(msg string, kv ...any) {
	if s.Logger != nil {
		s.Logger.Warnw(msg, kv...)
	}
}

// startDeploy : passe le conteneur à STARTING (écriture conditionnelle, refusée
// s'il est en suppression) puis lance le workflow de mise à jour. Si Temporal
// le refuse, le statut précédent est rétabli ; si un autre déploiement est déjà
// lancé, son statut est laissé tel quel.
func (s *WorkloadService) startDeploy(ctx context.Context, w *domain.Workload, in workflows.DeployWorkloadInput) error {
	if err := s.Repo.UpdateStatus(ctx, w.ID, utils.WorkloadStarting, utils.PhaseHelmChartLoading); err != nil {
		return err
	}
	_, err := s.Distributor.ExecuteWorkflow(ctx,
		temporal.ExecuteOptions{WorkflowID: workflows.WorkflowIDUpdatePrefix + w.ID.String()},
		workflows.DeployWorkloadWorkflow, in)
	if err == nil {
		return nil
	}
	if temporal.IsAlreadyStarted(err) {
		// Un autre déploiement a pris la place : le statut lui appartient
		// (il a écrit le même STARTING et écrira l'état final). Rétablir
		// l'état lu avant la course afficherait RUNNING pendant son
		// déploiement.
		return domain.ErrDeployInProgress
	}
	status, phase := w.Status, w.CurrentPhase
	if status == utils.WorkloadStarting {
		// Aucun workflow ne terminera ce STARTING.
		status, phase = utils.WorkloadFailed, utils.PhaseHelmError
	}
	s.compensate(ctx, "workload status not restored", w.ID, func(ctx context.Context) error {
		return s.Repo.UpdateStatus(ctx, w.ID, status, phase)
	})
	return err
}

// CreateAwaitingBuild : mêmes contrôles et même réservation de quota qu'un
// déploiement, mais sans image ni installation : le conteneur attend le build
// qui le déploiera (DeployBuiltImage).
func (s *WorkloadService) CreateAwaitingBuild(ctx context.Context, in *WorkloadServiceRequest) (*domain.Workload, error) {
	in.Image = ""
	workload, _, err := s.create(ctx, in, utils.WorkloadBuilding, utils.PhaseBuildRunning)
	return workload, err
}

// Remove : annule une création dont la suite a échoué (rien n'est installé).
func (s *WorkloadService) Remove(ctx context.Context, id uuid.UUID) error {
	return s.Repo.Delete(ctx, id)
}

// MarkAwaitingBuild : conteneur jamais déployé, nouvelle image en construction.
func (s *WorkloadService) MarkAwaitingBuild(ctx context.Context, w *domain.Workload) error {
	if w.Image != "" {
		return nil
	}
	return s.Repo.UpdateStatus(ctx, w.ID, utils.WorkloadBuilding, utils.PhaseBuildRunning)
}

// DeployBuiltImage : image construite par le serveur (push, « déployer
// maintenant »). Même logique qu'une mise à jour, contrôle de l'image compris ;
// le projet du build doit être celui du conteneur. Un conteneur arrêté n'est
// pas relancé : le déployer le redémarrerait.
func (s *WorkloadService) DeployBuiltImage(ctx context.Context, project, id uuid.UUID, image string) error {
	current, err := s.Repo.GetByID(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return buildsDomain.ErrWorkloadGone
	}
	if err != nil {
		return err
	}
	if current.ProjectID != project {
		return buildsDomain.ErrWorkloadGone
	}
	if err := s.canRun(ctx, project); err != nil {
		return err
	}
	switch current.Status {
	case utils.WorkloadStopped:
		return buildsDomain.ErrWorkloadStopped
	case utils.WorkloadDeleting, utils.WorkloadDeleted:
		return buildsDomain.ErrWorkloadGone
	}
	err = s.Update(ctx, id.String(), domain.UpdateWorkloadRequest{Image: image})
	switch {
	case errors.Is(err, domain.ErrDeployInProgress):
		return buildsDomain.ErrWorkloadBusy
	case errors.Is(err, domain.ErrWorkloadDeleting):
		return buildsDomain.ErrWorkloadGone
	}
	return err
}

// MarkBuildFailed : la première image n'a pas pu être construite. Un conteneur
// déjà en service n'est pas touché.
func (s *WorkloadService) MarkBuildFailed(ctx context.Context, project, id uuid.UUID) error {
	current, err := s.Repo.GetByID(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil || current.ProjectID != project || current.Image != "" || current.Status == utils.WorkloadDeleting {
		return err
	}
	err = s.Repo.UpdateStatus(ctx, id, utils.WorkloadFailed, utils.PhaseBuildFailed)
	if errors.Is(err, domain.ErrWorkloadDeleting) {
		return nil
	}
	return err
}

// ensureNotDeleting : un conteneur en suppression n'accepte plus aucune opération.
func ensureNotDeleting(w *domain.Workload) error {
	if w.Status == utils.WorkloadDeleting || w.Status == utils.WorkloadDeleted {
		return domain.ErrWorkloadDeleting
	}
	return nil
}

// ensureBuilt : un conteneur sans image n'a encore rien à arrêter, relancer ou
// déployer.
func ensureBuilt(w *domain.Workload) error {
	if w.Image == "" {
		return domain.ErrWorkloadNotBuilt
	}
	return nil
}

func (s *WorkloadService) create(ctx context.Context, in *WorkloadServiceRequest, status, phase string) (*domain.Workload, domain.CreateWorkloadRequest, error) {
	var none domain.CreateWorkloadRequest
	pID, _ := uuid.Parse(in.ProjectID)
	if err := s.canRun(ctx, pID); err != nil {
		return nil, none, err
	}

	// Une panne ne doit pas compter pour 0 : le contrôle laisserait tout passer.
	currentCPU, currentMem, currentStorage, err := s.Repo.GetTotalUsageByProject(ctx, pID)
	if err != nil {
		return nil, none, fmt.Errorf("project usage: %w", err)
	}

	project, err := s.ProjectRepo.GetProjectByID(ctx, pID.String())
	if err != nil {
		return nil, none, betoerrors.New(betoerrors.CodeNotFound, "project not found")
	}

	if in.Replicas <= 0 {
		// Non précisé : le quota comptait déjà 1, le déploiement partait à 0.
		in.Replicas = 1
	}

	// Sorte et processus : port, horaire, commande (worker et cron : sans port).
	if err := domain.NormalizeProcess(&in.CreateWorkloadRequest); err != nil {
		return nil, none, err // erreurs nommées : 400 traduit (router/errors.go)
	}
	replicas := int64(in.Replicas) // après NormalizeProcess : cron, service ramenés à une instance
	if in.Kind == domain.KindWeb || in.Kind == domain.KindService {
		if err := s.validateNetwork(in.TargetPort); err != nil {
			return nil, none, err
		}
	}
	if err := normalizeResources(&in.CreateWorkloadRequest); err != nil {
		return nil, none, err
	}

	requestedTotalCPU := s.parseCPU(in.CPULimit) * replicas
	limitCPU := s.parseCPU(project.CpuLimit)
	if (currentCPU + requestedTotalCPU) > limitCPU {
		return nil, none, betoerrors.Newf(betoerrors.CodeInvalidInput, "quota exceeded: CPU limit reached (%d/%d m)", currentCPU+requestedTotalCPU, limitCPU)
	}

	requestedTotalMem := s.parseMemory(in.MemoryLimit) * replicas
	limitMem := s.parseMemory(project.MemoryLimit)
	if (currentMem + requestedTotalMem) > limitMem {
		return nil, none, betoerrors.New(betoerrors.CodeInvalidInput, "quota exceeded: Memory limit reached")
	}

	if in.PersistenceEnabled {
		requestedStorage := s.parseStorage(in.StorageSize)
		limitStorage := s.parseStorage(project.StorageLimit)
		if (currentStorage + requestedStorage) > limitStorage {
			return nil, none, betoerrors.New(betoerrors.CodeInvalidInput, "quota exceeded: Storage limit reached")
		}
	}

	_ = currentStorage

	existing, err := s.Repo.ListByProject(ctx, pID)
	if err != nil {
		return nil, none, err
	}
	for _, w := range existing {
		if w.Name == in.Name {
			return nil, none, ErrWorkloadNameTaken
		}
	}

	if err := s.checkImage(pID, in.Image); err != nil {
		return nil, none, err
	}
	variables := domain.VariableChange{Env: in.EnvVars, Secrets: in.SecretData}
	if err := variables.Validate(); err != nil {
		return nil, none, err
	}
	targetNamespace := project.Namespace

	// Identité : vérifiée avant toute création (actAs sur le compte).
	var runAs *uuid.UUID
	if in.ServiceAccount != "" {
		if s.Identity == nil {
			return nil, none, ErrWorkloadIdentityDisabled
		}
		saID, err := s.Identity.Authorizer.AuthorizeRunAs(ctx, in.Caller, pID, in.ServiceAccount)
		if err != nil {
			return nil, none, err
		}
		runAs = &saID
	}

	// Classe des disques : celle du cluster du projet si le client n'en
	// demande pas d'autre ; gardée ensuite (un disque ne change pas de classe).
	prof, err := s.profileFor(ctx, pID)
	if err != nil {
		return nil, none, err
	}
	if prof.StorageClass != "" && (in.StorageClass == "" || in.StorageClass == utils.DefaultStorageClass) {
		in.StorageClass = prof.StorageClass
	}

	workload := &domain.Workload{
		ID:                 uuid.New(),
		ProjectID:          pID,
		Name:               in.Name,
		Namespace:          targetNamespace,
		ChartName:          defaultChartName,
		Version:            defaultChartVersion,
		Image:              in.Image,
		Kind:               in.Kind,
		Replicas:           in.Replicas,
		CPULimit:           in.CPULimit,
		MemoryLimit:        in.MemoryLimit,
		TargetPort:         in.TargetPort,
		PersistenceEnabled: in.PersistenceEnabled,
		StorageSize:        in.StorageSize,
		StorageClass:       defaultIfEmpty(in.StorageClass, utils.DefaultStorageClass),
		TLSEnabled:         in.TLSEnabled,
		Status:             status,
		CurrentPhase:       phase,
		ServiceAccountID:   runAs,
		ManagedBy:          in.ManagedBy,
	}
	spec := specOf(in.CreateWorkloadRequest)
	if runAs != nil {
		spec.ServiceAccount = iam.ServiceAccountPrincipal(*runAs).String()
	}
	if workload.Spec, err = json.Marshal(spec); err != nil {
		return nil, none, err
	}
	workload.ExternalURL = routeFor(in.Kind, in.Name, pID, in.TLSEnabled, in.NoPublicURL, prof).URL

	if err := s.Repo.Create(ctx, workload); err != nil {
		if errors.Is(err, repository.ErrNameTaken) {
			return nil, none, ErrWorkloadNameTaken
		}
		return nil, none, err
	}

	// Valeurs chiffrées avant le workflow, qui ne reçoit que leur révision.
	if workload.EnvRevision, _, err = s.Variables.Apply(ctx, workload.ID, variables); err != nil {
		_ = s.Repo.Delete(ctx, workload.ID)
		return nil, none, err
	}
	return workload, spec, nil
}

// normalizeResources : limites valides et valeurs par défaut, posées une seule
// fois ici. Le contrôle du quota, la base et Helm voient ainsi la même chose
// (avant, sans limites : 0 au contrôle, 200m en base, 500m dans Helm).
func normalizeResources(in *domain.CreateWorkloadRequest) error {
	in.CPULimit = defaultIfEmpty(in.CPULimit, activities.DefaultCPULimit)
	if !positiveQuantity(in.CPULimit) {
		return betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid CPU limit: %s", in.CPULimit)
	}
	in.MemoryLimit = defaultIfEmpty(in.MemoryLimit, activities.DefaultMemoryLimit)
	if !positiveQuantity(in.MemoryLimit) {
		return betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid memory limit: %s", in.MemoryLimit)
	}
	if in.PersistenceEnabled {
		in.StorageSize = defaultIfEmpty(in.StorageSize, utils.DefaultStorageSize)
		if !positiveQuantity(in.StorageSize) {
			return betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid storage size: %s", in.StorageSize)
		}
	}
	return nil
}

func positiveQuantity(v string) bool {
	q, err := resource.ParseQuantity(v)
	return err == nil && q.Sign() > 0
}

// Update : nouvelle version (image, disque, variables). Le reste de la
// configuration est repris du workload enregistré : avant, le workflow était
// relancé avec la seule image, et perdait port, ressources, réplicas, TLS,
// URL publique et variables d'environnement.
// Update : modification d'un conteneur ; une nouvelle image est ensuite
// reprise par les conteneurs qui la suivent (image_from).
func (s *WorkloadService) Update(ctx context.Context, id string, req domain.UpdateWorkloadRequest) error {
	if err := s.update(ctx, id, req); err != nil {
		return err
	}
	if req.Image != "" {
		s.propagateImage(ctx, id, req.Image)
	}
	return nil
}

func (s *WorkloadService) update(ctx context.Context, id string, req domain.UpdateWorkloadRequest) error {
	wID, err := uuid.Parse(id)
	if err != nil {
		return betoerrors.New(betoerrors.CodeNotFound, "workload not found")
	}
	current, err := s.Repo.GetByID(ctx, wID)
	if err != nil {
		return err
	}
	if err := s.canRun(ctx, current.ProjectID); err != nil {
		return err
	}
	if err := ensureNotDeleting(current); err != nil {
		return err
	}
	if err := s.ensureNoDeployRunning(ctx, current.ID); err != nil {
		return err
	}
	if req.Image == "" {
		if err := ensureBuilt(current); err != nil {
			return err
		}
	}
	variables := domain.VariableChange{Env: req.EnvVars, Secrets: req.SecretData, ReplaceEnv: req.EnvVars != nil}
	if err := variables.Validate(); err != nil {
		return err
	}

	if req.StorageSize != "" && current.StorageSize != "" {
		if utils.ParseStorageToBytes(req.StorageSize) < utils.ParseStorageToBytes(current.StorageSize) {
			return betoerrors.New(betoerrors.CodeInvalidInput, "reduction of storage size is not supported")
		}
	}
	spec, err := s.specFor(current)
	if err != nil {
		return err
	}
	if err := applyProcess(current, &spec, &req); err != nil {
		return err
	}
	if req.Image != "" {
		if err := s.checkImage(current.ProjectID, req.Image); err != nil {
			return err
		}
		current.Image, spec.Image = req.Image, req.Image
	}
	runtimeChanged, err := s.applyRuntime(ctx, current, &spec, req)
	if spec.Kind == domain.KindCron || spec.Kind == domain.KindService {
		// Cron : une exécution à la fois ; service : un seul pod sur son disque.
		current.Replicas, spec.Replicas = 1, 1
	}
	if err != nil {
		return err
	}
	if req.StorageSize != "" {
		current.StorageSize, spec.StorageSize = req.StorageSize, req.StorageSize
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	edit, err := s.applyVariables(ctx, current, variables)
	if err != nil {
		return err
	}
	current.EnvRevision = edit.revision
	in, prof, err := s.inputFor(ctx, current, spec)
	if err == nil {
		// Le workflow d'abord : si un autre déploiement a pris la place ou si
		// Temporal est indisponible, la base ne doit pas annoncer une version
		// qui ne sera jamais déployée.
		err = s.startDeploy(ctx, current, in)
	}
	if err != nil {
		// Refusée : les variables ne doivent pas survivre à la requête (le
		// déploiement gagnant les aurait lues et déployées).
		s.undoVariables(ctx, current.ID, edit)
		return err
	}
	// Lancé : la nouvelle version est enregistrée même si la requête s'interrompt.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	if runtimeChanged {
		if err := s.Repo.UpdateRuntime(save, current.ID, current.Replicas, current.CPULimit, current.MemoryLimit, current.TargetPort, current.TLSEnabled); err != nil {
			return err
		}
	}
	url := routeFor(spec.Kind, current.Name, current.ProjectID, spec.TLSEnabled, spec.NoPublicURL, prof).URL
	return s.Repo.UpdateSpec(save, current.ID, current.Image, current.StorageSize, url, raw)
}

// AddWaitFor : ajoute target aux adresses attendues avant le démarrage. Seule
// la spec change : le déploiement suivant (le branchement en lance un, en
// posant la variable) rend le conteneur d'initialisation.
func (s *WorkloadService) AddWaitFor(ctx context.Context, id uuid.UUID, target string) error {
	if !domain.ValidWaitFor([]string{target}) {
		return domain.ErrInvalidWaitFor
	}
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	spec, err := s.specFor(w)
	if err != nil {
		return err
	}
	if slices.Contains(spec.WaitFor, target) {
		return nil
	}
	if len(spec.WaitFor) >= domain.MaxWaitFor {
		return nil // déjà assez d'attentes : l'application se reconnectera seule
	}
	spec.WaitFor = append(spec.WaitFor, target)
	raw, err := json.Marshal(specOf(spec))
	if err != nil {
		return err
	}
	return s.Repo.UpdateSpec(ctx, id, w.Image, w.StorageSize, w.ExternalURL, raw)
}

// ensureNoDeployRunning : refus clair avant toute écriture. Le démarrage du
// workflow reste protégé si deux demandes passent ce contrôle ensemble.
func (s *WorkloadService) ensureNoDeployRunning(ctx context.Context, id uuid.UUID) error {
	for _, prefix := range []string{workflows.WorkflowIDDeployPrefix, workflows.WorkflowIDUpdatePrefix} {
		running, err := s.Distributor.IsRunning(ctx, prefix+id.String())
		if err != nil {
			return err
		}
		if running {
			return domain.ErrDeployInProgress
		}
	}
	deleting, err := s.Distributor.IsRunning(ctx, workflows.WorkflowIDDeletePrefix+id.String())
	if err != nil {
		return err
	}
	if deleting {
		return domain.ErrWorkloadDeleting
	}
	return nil
}

// variableEdit : modification appliquée, et de quoi l'annuler.
type variableEdit struct {
	before   domain.VariableSnapshot
	revision int64
	changed  bool
}

// applyVariables : importe au besoin les variables d'un conteneur antérieur,
// puis applique la modification si personne n'a écrit depuis la lecture : de
// deux requêtes concurrentes, la seconde est refusée sans rien écrire.
func (s *WorkloadService) applyVariables(ctx context.Context, w *domain.Workload, change domain.VariableChange) (variableEdit, error) {
	if w.EnvRevision == 0 {
		legacy, err := s.currentEnv(ctx, w)
		if err != nil {
			return variableEdit{}, err
		}
		if _, err := s.Variables.Import(ctx, w.ID, legacy); err != nil {
			return variableEdit{}, err
		}
	}
	before, err := s.Variables.Snapshot(ctx, w.ID)
	if err != nil {
		return variableEdit{}, err
	}
	change.IfRevision = before.Revision
	revision, changed, err := s.Variables.Apply(ctx, w.ID, change)
	if errors.Is(err, repository.ErrRevisionChanged) {
		return variableEdit{}, domain.ErrDeployInProgress
	}
	if err != nil {
		return variableEdit{}, err
	}
	return variableEdit{before: before, revision: revision, changed: changed}, nil
}

// undoVariables : remet les valeurs d'avant une modification dont le
// déploiement n'a pas démarré. Si une autre écriture l'a suivie, elle est
// laissée en place.
func (s *WorkloadService) undoVariables(ctx context.Context, id uuid.UUID, edit variableEdit) {
	if !edit.changed {
		return
	}
	s.compensate(ctx, "workload variables not restored", id, func(ctx context.Context) error {
		_, err := s.Variables.Restore(ctx, id, edit.before, edit.revision)
		return err
	})
}

// ListVariables : variables du conteneur, sans la valeur des secrets.
func (s *WorkloadService) ListVariables(ctx context.Context, id uuid.UUID) ([]domain.VariableView, error) {
	w, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.EnvRevision == 0 {
		legacy, err := s.currentEnv(ctx, w)
		if err != nil {
			return nil, err
		}
		if _, err := s.Variables.Import(ctx, w.ID, legacy); err != nil {
			return nil, err
		}
	}
	return s.Variables.List(ctx, id)
}

// PatchVariables : applique les modifications et redéploie si une valeur a
// changé (nouveaux pods). Un conteneur arrêté est refusé : le redéployer le
// redémarrerait.
func (s *WorkloadService) PatchVariables(ctx context.Context, id uuid.UUID, req domain.PatchVariablesRequest) (*domain.VariablesResponse, error) {
	current, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := ensureNotDeleting(current); err != nil {
		return nil, err
	}
	if current.Status == utils.WorkloadStopped {
		return nil, domain.ErrWorkloadStopped
	}
	if err := s.ensureNoDeployRunning(ctx, id); err != nil {
		return nil, err
	}
	change := domain.VariableChange{Env: req.Env, Secrets: req.Secrets, Unset: req.Unset}
	if err := change.Validate(); err != nil {
		return nil, err
	}
	edit, err := s.applyVariables(ctx, current, change)
	if err != nil {
		return nil, err
	}
	current.EnvRevision = edit.revision
	changed := edit.changed
	// Sans image : la valeur sera prise en compte au premier déploiement.
	// Renvoyer les mêmes valeurs ne change rien, mais doit relancer un
	// déploiement qui n'a jamais eu lieu (échec au lancement).
	if (changed || s.revisionBehind(ctx, current)) && current.Image != "" {
		if err := s.redeploy(ctx, current); err != nil {
			s.undoVariables(ctx, current.ID, edit)
			return nil, err
		}
	}
	views, err := s.Variables.List(ctx, id)
	if err != nil {
		return nil, err
	}
	return &domain.VariablesResponse{Variables: views, Changed: changed}, nil
}

// revisionBehind : le Deployment ne porte pas la révision courante des variables
// (ou n'existe pas). En cas de doute (cluster injoignable), on ne redéploie pas.
func (s *WorkloadService) revisionBehind(ctx context.Context, w *domain.Workload) bool {
	if s.Kube == nil || w.Image == "" {
		return false
	}
	kube, err := s.cluster(ctx, w)
	if err != nil {
		s.warn("deployed revision unavailable", "workload", w.ID, "error", err)
		return false
	}
	want := strconv.FormatInt(w.EnvRevision, 10)
	if isCron(w) {
		rev, found, err := cronRevision(ctx, kube.Client, w)
		if err != nil {
			s.warn("deployed revision unavailable", "workload", w.ID, "error", err)
			return false
		}
		return !found || rev != want
	}
	dep, err := kube.Client.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		s.warn("deployed revision unavailable", "workload", w.ID, "error", err)
		return false
	}
	return dep.Spec.Template.Annotations[utils.AnnotationEnvChecksum] != want
}

// specOf : spec déclarative enregistrée (sans variables ni secrets, qui
// restent dans le Secret Kubernetes du conteneur).
func specOf(req domain.CreateWorkloadRequest) domain.CreateWorkloadRequest {
	req.EnvVars, req.SecretData = nil, nil
	return req
}

// specFor : spec enregistrée ; conteneur antérieur à la spec : reconstituée
// depuis ses colonnes.
func (s *WorkloadService) specFor(w *domain.Workload) (domain.CreateWorkloadRequest, error) {
	var spec domain.CreateWorkloadRequest
	if len(w.Spec) > 0 {
		if err := json.Unmarshal(w.Spec, &spec); err != nil {
			return spec, fmt.Errorf("workload spec: %w", err)
		}
		spec.Kind = kindOf(w) // la colonne fait foi (immuable)
		return spec, nil
	}
	return domain.CreateWorkloadRequest{ProjectID: w.ProjectID.String(), Name: w.Name, Image: w.Image, Replicas: w.Replicas, Kind: kindOf(w),
		CPURequest: w.CPURequest, MemoryRequest: w.MemoryRequest, CPULimit: w.CPULimit, MemoryLimit: w.MemoryLimit,
		TargetPort: w.TargetPort, PersistenceEnabled: w.PersistenceEnabled, StorageSize: w.StorageSize,
		StorageClass: w.StorageClass, TLSEnabled: w.TLSEnabled}, nil
}

// applyRuntime : réplicas, limites, port, TLS et disque demandés, vérifiés
// contre les quotas du projet : une mise à jour ne doit pas permettre de
// dépasser ce qu'une création refuserait.
func (s *WorkloadService) applyRuntime(ctx context.Context, w *domain.Workload, spec *domain.CreateWorkloadRequest, req domain.UpdateWorkloadRequest) (bool, error) {
	if req.TargetPort != 0 {
		if err := s.validateNetwork(req.TargetPort); err != nil {
			return false, err
		}
	}
	replicas, cpu, mem := int64(max(w.Replicas, 1)), w.CPULimit, w.MemoryLimit
	newReplicas, newCPU, newMem := replicas, cpu, mem
	if req.Replicas < 0 {
		return false, betoerrors.New(betoerrors.CodeInvalidInput, "replicas must be positive")
	}
	if req.Replicas > 0 {
		newReplicas = int64(req.Replicas)
	}
	// Une valeur illisible compterait pour 0 dans le quota.
	if req.CPULimit != "" {
		if _, err := resource.ParseQuantity(req.CPULimit); err != nil {
			return false, betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid CPU limit: %s", req.CPULimit)
		}
		newCPU = req.CPULimit
	}
	if req.MemoryLimit != "" {
		if _, err := resource.ParseQuantity(req.MemoryLimit); err != nil {
			return false, betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid memory limit: %s", req.MemoryLimit)
		}
		newMem = req.MemoryLimit
	}
	growsDisk := req.StorageSize != "" && w.PersistenceEnabled && req.StorageSize != w.StorageSize
	if newReplicas != replicas || newCPU != cpu || newMem != mem || growsDisk {
		usedCPU, usedMem, usedStorage, err := s.Repo.GetTotalUsageByProject(ctx, w.ProjectID)
		if err != nil {
			return false, err
		}
		project, err := s.ProjectRepo.GetProjectByID(ctx, w.ProjectID.String())
		if err != nil {
			return false, betoerrors.New(betoerrors.CodeNotFound, "project not found")
		}
		totalCPU := usedCPU - s.parseCPU(cpu)*replicas + s.parseCPU(newCPU)*newReplicas
		if limit := s.parseCPU(project.CpuLimit); totalCPU > limit {
			return false, betoerrors.Newf(betoerrors.CodeInvalidInput, "quota exceeded: CPU limit reached (%d/%d m)", totalCPU, limit)
		}
		if usedMem-s.parseMemory(mem)*replicas+s.parseMemory(newMem)*newReplicas > s.parseMemory(project.MemoryLimit) {
			return false, betoerrors.New(betoerrors.CodeInvalidInput, "quota exceeded: Memory limit reached")
		}
		if growsDisk && usedStorage-s.parseStorage(w.StorageSize)+s.parseStorage(req.StorageSize) > s.parseStorage(project.StorageLimit) {
			return false, betoerrors.New(betoerrors.CodeInvalidInput, "quota exceeded: Storage limit reached")
		}
	}
	changed := newReplicas != replicas || newCPU != cpu || newMem != mem
	if newReplicas != replicas {
		w.Replicas, spec.Replicas = int(newReplicas), int(newReplicas)
	}
	if newCPU != cpu {
		w.CPULimit, spec.CPULimit = newCPU, newCPU
	}
	if newMem != mem {
		w.MemoryLimit, spec.MemoryLimit = newMem, newMem
	}
	if req.TargetPort != 0 && req.TargetPort != w.TargetPort {
		w.TargetPort, spec.TargetPort, changed = req.TargetPort, req.TargetPort, true
	}
	if req.TLSEnabled != nil && *req.TLSEnabled != w.TLSEnabled {
		w.TLSEnabled, spec.TLSEnabled, changed = *req.TLSEnabled, *req.TLSEnabled, true
	}
	if req.NoPublicURL != nil && *req.NoPublicURL != spec.NoPublicURL {
		spec.NoPublicURL, changed = *req.NoPublicURL, true
	}
	if req.ImageFrom != nil && *req.ImageFrom != spec.ImageFrom {
		// Pas un changement d'exécution : enregistré avec la spec, sans redéploiement propre.
		spec.ImageFrom = *req.ImageFrom
	}
	return changed, nil
}

// deployInput : entrée du workflow de déploiement, construite de la même
// façon à la création et à chaque mise à jour.
func (s *WorkloadService) deployInput(w *domain.Workload, spec domain.CreateWorkloadRequest, prof profile) workflows.DeployWorkloadInput {
	route := routeFor(spec.Kind, w.Name, w.ProjectID, spec.TLSEnabled, spec.NoPublicURL, prof)
	in := workflows.DeployWorkloadInput{
		Process: activities.ProcessSpec{
			Kind: kindOf(w), Command: spec.Command, Args: spec.Args, HealthCommand: spec.HealthCommand,
			Schedule: spec.Schedule, TimeZone: spec.TimeZone, MaxDurationSeconds: spec.MaxDurationSeconds,
			WaitFor: spec.WaitFor,
			// Cron arrêté : reste suspendu à chaque déploiement (mise à jour,
			// variables, retour arrière).
			Suspend: kindOf(w) == domain.KindCron && w.Status == utils.WorkloadStopped,
		},
		WorkloadID: w.ID.String(), ProjectID: w.ProjectID.String(), Namespace: w.Namespace, ReleaseName: w.Name,
		Image: w.Image, EnvRevision: w.EnvRevision, Replicas: spec.Replicas,
		PersistenceEnabled: spec.PersistenceEnabled, StorageSize: w.StorageSize, StorageClass: spec.StorageClass,
		MountPath: spec.MountPath, TargetPort: spec.TargetPort, ServiceType: spec.ServiceType,
		CPURequest: spec.CPURequest, MemoryRequest: spec.MemoryRequest,
		// Spec antérieure sans limites : celles de la base, comptées au quota.
		CPULimit: defaultIfEmpty(spec.CPULimit, w.CPULimit), MemoryLimit: defaultIfEmpty(spec.MemoryLimit, w.MemoryLimit),
		RunAsUser: spec.RunAsUser, RunAsGroup: spec.RunAsGroup, ReadOnlyRootFS: spec.ReadOnlyRootFS,
		PDBEnabled: spec.PDBEnabled, TLSEnabled: spec.TLSEnabled, HealthPath: spec.HealthPath, ReadinessPath: spec.ReadinessPath,
		StartupTimeoutSeconds: spec.StartupTimeoutSeconds,
		ExternalHost:          route.Host, IngressClass: route.IngressClass, IngressNamespace: route.IngressNamespace,
		Identity:      s.currentIdentity(w),
		PlatformImage: s.Registry != nil && registry.IsPlatformImage(s.Registry.Host, w.Image),
	}
	if a := spec.Autoscaling; a != nil && a.Enabled {
		in.AutoscalingEnabled, in.AutoscalingMin, in.AutoscalingMax = true, a.MinReplicas, a.MaxReplicas
		in.AutoscalingTargetCPU = a.TargetCPUUtilizationPercentage
	}
	return in
}

// currentEnv : variables d'environnement en place (secret <nom>-env créé au
// déploiement) ; aucune si le secret n'existe pas.
func (s *WorkloadService) currentEnv(ctx context.Context, w *domain.Workload) (map[string]string, error) {
	if s.Kube == nil {
		return nil, nil
	}
	kube, err := s.cluster(ctx, w)
	if err != nil {
		return nil, err
	}
	secret, err := kube.Client.CoreV1().Secrets(w.Namespace).Get(ctx, utils.EnvSecretName(w.Name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read current environment: %w", err)
	}
	env := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		env[k] = string(v)
	}
	return env, nil
}

// route : exposition publique d'un workload.
type route struct {
	Host             string // vide : pas d'ingress
	URL              string // enregistrée avec le workload, affichée à l'utilisateur
	IngressClass     string
	IngressNamespace string
}

// publicRoute : <nom>-<projet>.<ip>.sslip.io si l'IP du point d'entrée est
// configurée (vps.vps_ip) ; sinon aucune URL publique (avant : hôte
// « web-xxxxxx..sslip.io » invalide, refusé par Kubernetes).
// routeFor : adresse publique d'un conteneur web ; aucune pour un worker ou
// un cron (ni ingress, ni URL). noPublic : pas d'adresse sslip.io, mais la
// classe d'ingress reste connue (domaines vérifiés, politique réseau).
func routeFor(kind, name string, projectID uuid.UUID, tls, noPublic bool, prof profile) route {
	if kind != "" && kind != domain.KindWeb {
		return route{}
	}
	r := publicRoute(name, projectID, tls, prof)
	if noPublic {
		r.Host, r.URL = "", ""
	}
	return r
}

// kindOf : sorte enregistrée (vide : conteneur antérieur, donc web).
func kindOf(w *domain.Workload) string {
	if w.Kind == "" {
		return domain.KindWeb
	}
	return w.Kind
}

// applyProcess : commande, arguments et horaire demandés. La sorte ne change
// jamais ; port et TLS n'ont pas de sens hors web (ignorés : un client peut
// les envoyer par défaut, comme la CLI ou MCP).
func applyProcess(w *domain.Workload, spec *domain.CreateWorkloadRequest, req *domain.UpdateWorkloadRequest) error {
	kind := kindOf(w)
	switch kind {
	case domain.KindWeb:
	case domain.KindService:
		// Le port reste réglable (c'est celui du Service) ; jamais public.
		req.TLSEnabled, req.NoPublicURL = nil, nil
		if req.Replicas > 1 {
			return domain.ErrSingleInstance
		}
	default:
		req.TargetPort, req.TLSEnabled, req.NoPublicURL = 0, nil, nil
	}
	if req.StartupTimeoutSeconds != nil {
		if !domain.ValidStartupTimeout(*req.StartupTimeoutSeconds) {
			return domain.ErrStartupTimeout
		}
		spec.StartupTimeoutSeconds = *req.StartupTimeoutSeconds
	}
	if req.Command != nil {
		if !domain.ValidCommand(*req.Command) {
			return domain.ErrInvalidCommand
		}
		spec.Command = *req.Command
	}
	if req.HealthCommand != nil && (kind == domain.KindWorker || kind == domain.KindService) {
		if !domain.ValidCommand(*req.HealthCommand) {
			return domain.ErrInvalidCommand
		}
		spec.HealthCommand = *req.HealthCommand
	}
	if req.HealthPath != nil {
		if kind != domain.KindWeb {
			return domain.ErrNotWeb
		}
		if !domain.ValidHealthPath(*req.HealthPath) {
			return domain.ErrInvalidHealthPath
		}
		// Les trois sondes suivent le nouveau chemin.
		spec.HealthPath, spec.ReadinessPath = *req.HealthPath, *req.HealthPath
	}
	if req.Args != nil {
		if !domain.ValidCommand(*req.Args) {
			return domain.ErrInvalidCommand
		}
		spec.Args = *req.Args
	}
	if req.Schedule != "" {
		if kind != domain.KindCron {
			return domain.ErrNotCron
		}
		if !domain.ValidSchedule(req.Schedule) {
			return domain.ErrInvalidSchedule
		}
		spec.Schedule = strings.TrimSpace(req.Schedule)
	}
	return nil
}

func publicRoute(name string, projectID uuid.UUID, tls bool, prof profile) route {
	r := route{IngressClass: prof.IngressClass, IngressNamespace: prof.IngressNamespace}
	ip := prof.PublicIP
	if ip == "" {
		return r
	}
	r.Host = fmt.Sprintf("%s-%s.%s.%s", name, projectID.String()[:publicRouteProjectIDLen], ip, publicRouteDomain)
	scheme := schemeHTTP
	if tls {
		scheme = schemeHTTPS
	}
	r.URL = scheme + "://" + r.Host
	return r
}

func (s *WorkloadService) parseCPU(cpu string) int64 {
	res, err := resource.ParseQuantity(cpu)
	if err != nil {
		return 0
	}
	return res.MilliValue() // milli-cpu ( 1 = 1000m)
}

func (s *WorkloadService) parseMemory(mem string) int64 {
	res, err := resource.ParseQuantity(mem)
	if err != nil {
		return 0
	}
	return res.Value() / utils.BytesPerMi //  en Mi
}

func (s *WorkloadService) parseStorage(storage string) int64 {
	if storage == "" {
		return 0
	}
	res, err := resource.ParseQuantity(storage)
	if err != nil {
		return 0
	}
	return res.Value() / utils.BytesPerMi
}

func (s *WorkloadService) validateNetwork(port int) error {

	if port <= 0 || port > maxTargetPort {
		return betoerrors.Newf(betoerrors.CodeInvalidInput, "invalid target port: %d (must be between 1 and 65535)", port)
	}
	// Pas de liste de ports "réservés" : le port cible vit dans le network
	// namespace du pod, il ne peut pas entrer en collision avec les services
	// de la plateforme (API, Postgres, Temporal…).
	if port < privilegedPortLimit && port != httpPort {
		return betoerrors.New(betoerrors.CodeInvalidInput, "ports below 1024 (except 80) are system-privileged and not allowed")
	}

	return nil
}

// currentIdentity : identité conservée lors d'une mise à jour.
func (s *WorkloadService) currentIdentity(w *domain.Workload) *activities.WorkloadIdentity {
	if w.ServiceAccountID == nil || s.Identity == nil {
		return nil
	}
	return s.identity(*w.ServiceAccountID)
}
