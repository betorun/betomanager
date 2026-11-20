// Package addons : services additionnels d'un projet (Redis…), portés par un
// conteneur de sorte « service » créé à partir d'un modèle du catalogue.
package addons

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/background"
	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Projects : projet par identifiant (état, plafond de services).
type Projects interface {
	GetProjectByID(ctx context.Context, id string) (*dauth.Project, error)
}

// Repository : instances et branchements enregistrés.
type Repository interface {
	Create(ctx context.Context, a *domain.Addon) error
	List(ctx context.Context, projectID uuid.UUID) ([]domain.Addon, error)
	Get(ctx context.Context, projectID, id uuid.UUID) (*domain.Addon, error)
	Count(ctx context.Context, projectID uuid.UUID) (int64, error)
	SetVersion(ctx context.Context, id uuid.UUID, version string) error
	Delete(ctx context.Context, id uuid.UUID) error
	SaveAttachment(ctx context.Context, a *domain.Attachment) error
	ListAttachments(ctx context.Context, addonIDs []uuid.UUID) ([]domain.Attachment, error)
}

// Carrier : conteneur porteur d'une instance.
type Carrier struct {
	ProjectID  uuid.UUID
	Name       string
	Image      string
	Port       int
	Command    []string
	Health     []string
	CPURequest string
	CPULimit   string
	Memory     string
	// Disk : taille du disque ("" : sans disque) monté sur DiskPath.
	Disk      string
	DiskPath  string
	RunAsUser int64
	Env       map[string]string
	Secrets   map[string]string
	Caller    string
	// ManagedBy : marque du conteneur (« addon/redis/<id> ») : ses actions
	// sensibles ne passent plus que par ce module.
	ManagedBy string
}

// CarrierState : état du conteneur porteur.
type CarrierState struct {
	Status string
	Phase  string
}

// Workloads : les conteneurs vus par ce module (câblés au module workloads).
type Workloads interface {
	Deploy(ctx context.Context, c Carrier) (uuid.UUID, error)
	Delete(ctx context.Context, id uuid.UUID) error
	SetImage(ctx context.Context, id uuid.UUID, image string) error
	States(ctx context.Context, projectID uuid.UUID) (map[uuid.UUID]CarrierState, error)
	NameTaken(ctx context.Context, projectID uuid.UUID, name string) (bool, error)
	ProjectOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	NameOf(ctx context.Context, id uuid.UUID) (string, error)
	// Secret : valeur d'une variable secrète (mot de passe du service).
	Secret(ctx context.Context, id uuid.UUID, name string) (string, error)
	// SetSecrets : variables secrètes posées ensemble (un seul redéploiement).
	SetSecrets(ctx context.Context, id uuid.UUID, values map[string]string) (redeploy bool, err error)
	// WaitFor : le conteneur attendra host:port avant de démarrer.
	WaitFor(ctx context.Context, id uuid.UUID, target string) error
}

// Service : cycle de vie des services. Workloads nil = indisponible.
type Service struct {
	Repo      Repository
	Projects  Projects
	Workloads Workloads
	// Waitlists / Announcer : « Me prévenir » des services annoncés.
	Waitlists WaitlistStore
	Announcer Announcer
	// Backups : sauvegardes des services qui en ont (nil : indisponibles).
	Backups *BackupConfig
	Logger  *zap.SugaredLogger
	// Spawner : tâches de fond suivies par l'arrêt (nil : goroutine simple).
	Spawner background.Spawner
}

func (s *Service) available() error {
	if s.Workloads == nil {
		return domain.ErrUnavailable
	}
	return nil
}

// Catalog : modèles ouverts.
func (s *Service) Catalog() []domain.Template { return domain.Catalog }

// Create : vérifie la demande et le plafond, génère le mot de passe, crée le
// conteneur porteur puis enregistre l'instance. Échec de l'enregistrement :
// le conteneur est supprimé (rien d'orphelin ne reste facturé).
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in domain.CreateAddonRequest, creator string) (*domain.AddonView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	t, ok := domain.TemplateByID(strings.TrimSpace(in.Template))
	if !ok {
		return nil, domain.ErrUnknownTemplate
	}
	name := strings.TrimSpace(in.Name)
	if !domain.ValidName(name) {
		return nil, domain.ErrInvalidName
	}
	version, _, ok := t.Version(strings.TrimSpace(in.Version))
	if !ok {
		return nil, domain.ErrInvalidVersion
	}
	mode, ok := t.Mode(strings.TrimSpace(in.Mode))
	if !ok {
		return nil, domain.ErrInvalidMode
	}
	sizeID := strings.TrimSpace(in.Size)
	if sizeID == "" {
		sizeID = domain.DefaultSize
	}
	size, ok := domain.SizeByID(sizeID)
	if !ok {
		return nil, domain.ErrInvalidSize
	}
	disk := strings.TrimSpace(in.Disk)
	if mode.Persistent {
		if disk == "" {
			disk = t.Disks[0]
		}
		if !t.ValidDisk(disk) {
			return nil, domain.ErrInvalidDisk
		}
	} else if disk != "" {
		return nil, domain.ErrInvalidDisk // cache : sans disque
	}

	project, err := s.Projects.GetProjectByID(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	if project.Status != utils.ProjectStatusReady {
		return nil, fmt.Errorf("project is not ready (%s)", project.Status)
	}
	limit := project.MaxAddons
	if limit <= 0 {
		limit = domain.DefaultMaxPerProject
	}
	if n, err := s.Repo.Count(ctx, projectID); err != nil {
		return nil, err
	} else if n >= int64(limit) {
		return nil, domain.ErrLimitReached
	}
	if taken, err := s.Workloads.NameTaken(ctx, projectID, name); err != nil {
		return nil, err
	} else if taken {
		return nil, domain.ErrNameTaken
	}

	password, err := newPassword()
	if err != nil {
		return nil, err
	}
	proc := t.Process(mode, size)
	addonID := uuid.New()
	c := Carrier{
		ManagedBy: "addon/" + t.ID + "/" + addonID.String(),
		ProjectID: projectID, Name: name, Image: version.Image, Port: t.Port,
		Command: proc.Command, Health: proc.Health,
		CPURequest: size.CPURequest, CPULimit: size.CPULimit, Memory: size.Memory,
		DiskPath: t.DiskPath, RunAsUser: t.RunAsUser, Env: proc.Env,
		Secrets: map[string]string{domain.PasswordVariable: password},
		Caller:  creator,
	}
	for _, alias := range t.PasswordAliases {
		c.Secrets[alias] = password
	}
	if mode.Persistent {
		c.Disk = disk
	}
	workloadID, err := s.Workloads.Deploy(ctx, c)
	if err != nil {
		return nil, err
	}
	a := &domain.Addon{
		ID: addonID, ProjectID: projectID, WorkloadID: workloadID, Name: name, Template: t.ID,
		Version: version.ID, Mode: mode.ID, Size: size.ID, Disk: c.Disk, CreatedBy: creator,
	}
	if err := s.Repo.Create(ctx, a); err != nil {
		if derr := s.Workloads.Delete(context.WithoutCancel(ctx), workloadID); derr != nil {
			s.warn("addon: carrier left after failed registration", "workload", workloadID, "error", derr)
		}
		return nil, err
	}
	v := s.view(*a, CarrierState{Status: utils.WorkloadStarting}, nil)
	return &v, nil
}

// List : services du projet, avec l'état de leur conteneur et leurs branchements.
func (s *Service) List(ctx context.Context, projectID uuid.UUID) (*domain.AddonList, error) {
	out := &domain.AddonList{Addons: []domain.AddonView{}, Max: domain.DefaultMaxPerProject}
	if p, err := s.Projects.GetProjectByID(ctx, projectID.String()); err == nil && p.MaxAddons > 0 {
		out.Max = p.MaxAddons
	}
	rows, err := s.Repo.List(ctx, projectID)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	states := map[uuid.UUID]CarrierState{}
	if s.Workloads != nil {
		if states, err = s.Workloads.States(ctx, projectID); err != nil {
			return nil, err
		}
	}
	attached, err := s.attachments(ctx, rows)
	if err != nil {
		return nil, err
	}
	for _, a := range rows {
		out.Addons = append(out.Addons, s.view(a, states[a.WorkloadID], attached[a.ID]))
	}
	return out, nil
}

// Get : un service du projet.
func (s *Service) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.AddonView, error) {
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	var st CarrierState
	if s.Workloads != nil {
		states, err := s.Workloads.States(ctx, projectID)
		if err != nil {
			return nil, err
		}
		st = states[a.WorkloadID]
	}
	attached, err := s.attachments(ctx, []domain.Addon{*a})
	if err != nil {
		return nil, err
	}
	v := s.view(*a, st, attached[a.ID])
	return &v, nil
}

// Upgrade : passe à une version plus récente maintenue (défaut : la
// dernière). Jamais de retour en arrière. Le conteneur porteur est redéployé
// sur la nouvelle image ; ses données (disque) sont gardées.
func (s *Service) Upgrade(ctx context.Context, projectID, id uuid.UUID, target string) (*domain.AddonView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	t, ok := domain.TemplateByID(a.Template)
	if !ok {
		return nil, domain.ErrUnknownTemplate
	}
	if _, _, ok := t.Version(strings.TrimSpace(target)); !ok {
		return nil, domain.ErrInvalidVersion
	}
	next, newer := t.Newer(a.Version, strings.TrimSpace(target))
	if !newer {
		return nil, domain.ErrNoUpgrade
	}
	if err := s.Workloads.SetImage(ctx, a.WorkloadID, next.Image); err != nil {
		return nil, err
	}
	if err := s.Repo.SetVersion(ctx, a.ID, next.ID); err != nil {
		return nil, err
	}
	return s.Get(ctx, projectID, id)
}

// Delete : supprime le conteneur porteur (et son disque) puis l'instance.
// Les conteneurs branchés gardent leur variable : on les renvoie pour que
// la console prévienne.
func (s *Service) Delete(ctx context.Context, projectID, id uuid.UUID) (*domain.DeleteResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	attached, err := s.attachments(ctx, []domain.Addon{*a})
	if err != nil {
		return nil, err
	}
	if err := s.Workloads.Delete(ctx, a.WorkloadID); err != nil {
		return nil, err
	}
	if err := s.Repo.Delete(ctx, a.ID); err != nil {
		return nil, err
	}
	s.dropBackups(context.WithoutCancel(ctx), a.ID)
	still := attached[a.ID]
	if still == nil {
		still = []domain.Attachment{}
	}
	return &domain.DeleteResponse{StillAttached: still}, nil
}

// Attach : pose l'adresse de connexion (mot de passe compris) dans une
// variable SECRÈTE du conteneur, qui est redéployé. Le conteneur doit être
// du même projet ; l'adresse ne sort jamais de l'API. Les droits (connect
// sur le service, variables du conteneur) sont vérifiés avant l'appel.
func (s *Service) Attach(ctx context.Context, projectID, id, workloadID uuid.UUID, variable string) (*domain.AttachResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	a, err := s.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	t, ok := domain.TemplateByID(a.Template)
	if !ok {
		return nil, domain.ErrUnknownTemplate
	}
	if variable == "" {
		variable = t.Variable
	}
	keyVariable := t.KeyVariable(variable)
	if !domain.ValidVariable(variable) || (keyVariable != "" && !domain.ValidVariable(keyVariable)) {
		return nil, domain.ErrInvalidVariable
	}
	if workloadID == a.WorkloadID {
		return nil, domain.ErrSelfAttach
	}
	owner, err := s.Workloads.ProjectOf(ctx, workloadID)
	if err != nil || owner != projectID {
		return nil, domain.ErrWorkloadNotFound
	}
	password, err := s.Workloads.Secret(ctx, a.WorkloadID, domain.PasswordVariable)
	if err != nil || password == "" {
		return nil, domain.ErrNotReady
	}
	// Attente du service au démarrage, puis la variable (qui redéploie).
	if err := s.Workloads.WaitFor(ctx, workloadID, a.Name+":"+strconv.Itoa(t.Port)); err != nil {
		s.warn("addon: wait-for not set", "workload", workloadID, "error", err)
	}
	redeploy, err := s.Workloads.SetSecrets(ctx, workloadID, t.Connection(variable, a.Name, password))
	if err != nil {
		return nil, err
	}
	s.recordAttachment(ctx, a.ID, workloadID, variable)
	return &domain.AttachResponse{WorkloadID: workloadID.String(), Variable: variable, KeyVariable: keyVariable, Redeploy: redeploy}, nil
}

// recordAttachment : un échec n'annule pas le branchement (déjà posé) ; la
// carte ne le montrera simplement pas.
func (s *Service) recordAttachment(ctx context.Context, addonID, workloadID uuid.UUID, variable string) {
	name, err := s.Workloads.NameOf(ctx, workloadID)
	if err == nil {
		err = s.Repo.SaveAttachment(ctx, &domain.Attachment{AddonID: addonID, WorkloadID: workloadID, WorkloadName: name, Variable: variable})
	}
	if err != nil {
		s.warn("addon: attachment not recorded", "addon", addonID, "workload", workloadID, "error", err)
	}
}

func (s *Service) attachments(ctx context.Context, rows []domain.Addon) (map[uuid.UUID][]domain.Attachment, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, a := range rows {
		ids = append(ids, a.ID)
	}
	list, err := s.Repo.ListAttachments(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]domain.Attachment{}
	for _, at := range list {
		out[at.AddonID] = append(out[at.AddonID], at)
	}
	return out, nil
}

func (s *Service) view(a domain.Addon, st CarrierState, attached []domain.Attachment) domain.AddonView {
	if attached == nil {
		attached = []domain.Attachment{}
	}
	v := domain.AddonView{
		ID: a.ID.String(), Name: a.Name, Template: a.Template, Version: a.Version, Mode: a.Mode,
		Size: a.Size, Disk: a.Disk, Status: st.Status, Phase: st.Phase, WorkloadID: a.WorkloadID.String(),
		Host: a.Name, Attachments: attached, CreatedAt: a.CreatedAt,
	}
	if v.Status == "" {
		v.Status = "UNKNOWN"
	}
	if t, ok := domain.TemplateByID(a.Template); ok {
		v.Title, v.Logo, v.Port, v.Variable = t.Title, t.Logo, t.Port, t.Variable
		latest := t.Latest()
		v.LatestVersion = latest.ID
		if next, newer := t.Newer(a.Version, latest.ID); newer {
			v.UpgradeAvailable = true
			if cur, _, ok := t.Version(a.Version); ok && next.Major != cur.Major {
				v.MajorUpgrade = true
			}
		}
		v.Host = a.Name + ":" + strconv.Itoa(t.Port)
	}
	return v
}

// newPassword : 32 octets aléatoires en hexadécimal (rien à échapper dans
// une adresse redis:// ou mysql://).
func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("addon password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) warn(msg string, kv ...any) {
	if s.Logger != nil {
		s.Logger.Warnw(msg, kv...)
	}
}
