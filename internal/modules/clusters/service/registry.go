package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// Registre des clusters (socle multi-cluster, lot 1) : l'équipe déclare les
// clusters, dépose leur accès chiffré ; le worker relève leur santé.

// RegistryStore : persistance du registre (repository.RegistryStore).
type RegistryStore interface {
	List(ctx context.Context) ([]domain.Cluster, error)
	Get(ctx context.Context, clusterID string) (*domain.Cluster, error)
	Declare(ctx context.Context, c *domain.Cluster) error
	Update(ctx context.Context, clusterID string, fields map[string]any) error
	SetDefault(ctx context.Context, clusterID string) error
	EnsureDefault(ctx context.Context, clusterID string) error
	Delete(ctx context.Context, clusterID string) error
	Healths(ctx context.Context) (map[string]*domain.Health, error)
	SaveHealth(ctx context.Context, h *domain.Health) error
	ProjectCounts(ctx context.Context, defaultID string) (map[string]int, error)
}

// Sealer : chiffrement des kubeconfig (clé maître, auth.Sealer).
type Sealer interface {
	Seal(plaintext []byte, aad string) ([]byte, error)
	Open(ciphertext []byte, aad string) ([]byte, error)
}

// Reader : lecture d'un cluster par son API (repository.Kube).
type Reader interface {
	Snapshot(ctx context.Context) (*domain.Snapshot, error)
	Nodes(ctx context.Context) ([]domain.Node, error)
}

// Dialer : client d'un kubeconfig déposé ; rend aussi l'adresse du serveur.
// Refuse un kubeconfig qui lancerait une commande ou lirait un fichier local.
type Dialer func(raw []byte) (Reader, string, error)

// HealthObserver : santé relevée (métriques, alertes de l'équipe).
type HealthObserver interface {
	ClusterHealth(ctx context.Context, c *domain.Cluster, h *domain.Health, changed bool)
}

const (
	// DefaultHealthEvery : fréquence des relevés de santé (worker).
	DefaultHealthEvery = time.Minute
	// probeTimeout : un relevé d'un cluster ; probeConcurrency : clusters
	// relevés en parallèle.
	probeTimeout     = 20 * time.Second
	probeConcurrency = 4
)

// Registry : registre et santé des clusters.
type Registry struct {
	Store  RegistryStore
	Sealer Sealer // nil : aucun kubeconfig ne peut être déposé ni lu
	Dial   Dialer
	// DefaultID, Default : cluster de la configuration du déploiement (lu
	// par son kubeconfig de fichier ou en cluster) ; Default nil : inconnu.
	DefaultID string
	Default   Reader
	Observer  HealthObserver
	Logger    *zap.SugaredLogger
	Now       func() time.Time

	mu    sync.Mutex
	cache map[string]cachedReader
}

type cachedReader struct {
	setAt  time.Time
	reader Reader
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// ErrNoSealer : la clé maître manque (aucun kubeconfig ne peut être gardé).
var ErrNoSealer = errors.New("cluster access encryption is not configured")

// DeclareInput : nouveau cluster du registre.
type DeclareInput struct {
	ClusterID    string `json:"cluster_id" binding:"required" description:"Identifiant du cluster, celui de son watcher (3 à 63 minuscules, chiffres, tirets)"`
	Name         string `json:"name" description:"Nom affiché (80 caractères au plus)"`
	Provider     string `json:"provider" binding:"required" description:"Fournisseur de l'infrastructure : ovh, proxmox ou other"`
	Distribution string `json:"distribution" binding:"required" description:"Distribution Kubernetes : k3s ou k8s"`
	Region       string `json:"region" description:"Région (ex. eu-west-gra), minuscules, chiffres, tirets"`
	Notes        string `json:"notes" description:"Notes de l'équipe (2000 caractères au plus)"`
}

// UpdateInput : champs modifiés (absents : inchangés).
type UpdateInput struct {
	Name         *string `json:"name,omitempty" description:"Nom affiché"`
	Provider     *string `json:"provider,omitempty" description:"ovh, proxmox ou other"`
	Distribution *string `json:"distribution,omitempty" description:"k3s ou k8s"`
	Region       *string `json:"region,omitempty" description:"Région"`
	State        *string `json:"state,omitempty" description:"active (reçoit des projets), closed (n'en reçoit plus) ou disabled (hors service, plus sondé)"`
	Notes        *string `json:"notes,omitempty" description:"Notes de l'équipe"`
	// Réglages du cluster (vides : ceux de la configuration du déploiement).
	IngressClass     *string `json:"ingress_class,omitempty" description:"Classe d'ingress des domaines des projets (ex. traefik)"`
	IngressNamespace *string `json:"ingress_namespace,omitempty" description:"Namespace du contrôleur d'ingress (ex. kube-system)"`
	IngressTarget    *string `json:"ingress_target,omitempty" description:"Nom DNS ou adresse IPv4 vers laquelle pointent les domaines personnalisés"`
	StorageClass     *string `json:"storage_class,omitempty" description:"Classe des disques des projets (vide : celle par défaut du cluster)"`
	// EgressGateway : passerelle de sortie installée (IP dédiées des projets).
	EgressGateway *string `json:"egress_gateway,omitempty" description:"Passerelle de sortie installée sur le cluster : cilium, ou vide (pas d'IP de sortie dédiée)"`
}

// EnsureDefault : le cluster de la configuration figure au registre.
func (r *Registry) EnsureDefault(ctx context.Context) error {
	if strings.TrimSpace(r.DefaultID) == "" {
		return nil
	}
	return r.Store.EnsureDefault(ctx, r.DefaultID)
}

// List : registre et derniers relevés.
func (r *Registry) List(ctx context.Context) ([]domain.ClusterView, error) {
	cs, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	hs, err := r.Store.Healths(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := r.Store.ProjectCounts(ctx, r.defaultID(cs))
	if err != nil {
		return nil, err
	}
	out := make([]domain.ClusterView, 0, len(cs))
	for i := range cs {
		v := r.view(&cs[i], hs[cs[i].ClusterID])
		v.Projects = counts[cs[i].ClusterID]
		out = append(out, v)
	}
	return out, nil
}

// defaultID : cluster par défaut du registre ("" s'il n'y en a pas).
func (r *Registry) defaultID(cs []domain.Cluster) string {
	for _, c := range cs {
		if c.IsDefault {
			return c.ClusterID
		}
	}
	return ""
}

// Get : un cluster et son relevé.
func (r *Registry) Get(ctx context.Context, clusterID string) (*domain.ClusterView, error) {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	hs, err := r.Store.Healths(ctx)
	if err != nil {
		return nil, err
	}
	v := r.view(c, hs[clusterID])
	def := ""
	if c.IsDefault {
		def = c.ClusterID
	}
	if counts, err := r.Store.ProjectCounts(ctx, def); err == nil {
		v.Projects = counts[clusterID]
	}
	return &v, nil
}

func (r *Registry) view(c *domain.Cluster, h *domain.Health) domain.ClusterView {
	return domain.ClusterView{Cluster: *c, HasKubeconfig: len(c.KubeconfigCiphertext) > 0,
		FromConfig: len(c.KubeconfigCiphertext) == 0 && c.ClusterID == r.DefaultID && r.Default != nil, Health: h}
}

// Declare : nouveau cluster, sans accès (déposé ensuite).
func (r *Registry) Declare(ctx context.Context, in DeclareInput, by string) (*domain.ClusterView, error) {
	id := strings.TrimSpace(in.ClusterID)
	if !domain.ValidClusterID(id) {
		return nil, domain.ErrInvalidClusterID
	}
	c := &domain.Cluster{ClusterID: id, Name: strings.TrimSpace(in.Name), Provider: in.Provider, Distribution: in.Distribution,
		Region: strings.TrimSpace(in.Region), State: domain.StateActive, Notes: strings.TrimSpace(in.Notes), DeclaredBy: by}
	if c.Name == "" {
		c.Name = id
	}
	if err := domain.ValidateRegistry(c.Provider, c.Distribution, c.Region, c.State, c.Name, c.Notes); err != nil {
		return nil, err
	}
	if err := r.Store.Declare(ctx, c); err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

// Update : le cluster par défaut ne peut pas être mis hors service.
func (r *Registry) Update(ctx context.Context, clusterID string, in UpdateInput) (*domain.ClusterView, error) {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	set := func(dst *string, v *string, col string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
			fields[col] = *dst
		}
	}
	set(&c.Name, in.Name, "name")
	set(&c.Provider, in.Provider, "provider")
	set(&c.Distribution, in.Distribution, "distribution")
	set(&c.Region, in.Region, "region")
	set(&c.State, in.State, "state")
	set(&c.Notes, in.Notes, "notes")
	set(&c.IngressClass, in.IngressClass, "ingress_class")
	set(&c.IngressNamespace, in.IngressNamespace, "ingress_namespace")
	set(&c.IngressTarget, in.IngressTarget, "ingress_target")
	set(&c.StorageClass, in.StorageClass, "storage_class")
	set(&c.EgressGateway, in.EgressGateway, "egress_gateway")
	if !domain.ValidEgressGateway(c.EgressGateway) {
		return nil, domain.ErrInvalidEgressGateway
	}
	for _, v := range []string{c.IngressClass, c.IngressNamespace, c.IngressTarget, c.StorageClass} {
		if !domain.ValidSetting(v) {
			return nil, domain.ErrInvalidSetting
		}
	}
	if c.Name == "" {
		c.Name, fields["name"] = c.ClusterID, c.ClusterID
	}
	if err := domain.ValidateRegistry(c.Provider, c.Distribution, c.Region, c.State, c.Name, c.Notes); err != nil {
		return nil, err
	}
	if c.IsDefault && c.State == domain.StateDisabled {
		return nil, domain.ErrDefaultCluster
	}
	if len(fields) > 0 {
		if err := r.Store.Update(ctx, clusterID, fields); err != nil {
			return nil, err
		}
	}
	return r.Get(ctx, clusterID)
}

// SetKubeconfig : accès déposé seulement si le cluster répond avec lui ; il
// est chiffré (AAD = identifiant du cluster) et jamais rendu.
func (r *Registry) SetKubeconfig(ctx context.Context, clusterID string, raw []byte, by string) (*domain.ClusterView, error) {
	if r.Sealer == nil {
		return nil, ErrNoSealer
	}
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	reader, server, err := r.Dial(raw)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	snap, err := reader.Snapshot(pctx)
	if errors.Is(err, domain.ErrAccessDenied) {
		return nil, domain.ErrAccessDenied
	}
	if err != nil {
		return nil, domain.ErrClusterUnreachable
	}
	sealed, err := r.Sealer.Seal(raw, aad(clusterID))
	if err != nil {
		return nil, err
	}
	now := r.now()
	if err := r.Store.Update(ctx, clusterID, map[string]any{"kubeconfig_ciphertext": sealed, "api_server": server,
		"kubeconfig_set_at": now, "kubeconfig_set_by": by}); err != nil {
		return nil, err
	}
	c.KubeconfigCiphertext, c.APIServer, c.KubeconfigSetAt = sealed, server, &now
	r.remember(clusterID, now, reader)
	prev := r.prevHealth(ctx, clusterID)
	r.save(ctx, c, prev, domain.HealthFrom(c, prev, snap, nil, time.Since(start), now))
	return r.Get(ctx, clusterID)
}

// SetDefault : un cluster hors service ne peut pas devenir le défaut.
func (r *Registry) SetDefault(ctx context.Context, clusterID string) (*domain.ClusterView, error) {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if c.State == domain.StateDisabled {
		return nil, domain.ErrDisabledDefault
	}
	if err := r.Store.SetDefault(ctx, clusterID); err != nil {
		return nil, err
	}
	return r.Get(ctx, clusterID)
}

// Delete : retiré du registre (jamais le cluster par défaut). La clé de son
// watcher se révoque à part (clés des clusters).
func (r *Registry) Delete(ctx context.Context, clusterID string) error {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return err
	}
	if c.IsDefault {
		return domain.ErrDefaultCluster
	}
	counts, err := r.Store.ProjectCounts(ctx, "")
	if err != nil {
		return err
	}
	if counts[clusterID] > 0 {
		return domain.ErrClusterHasProjects
	}
	if err := r.Store.Delete(ctx, clusterID); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.cache, clusterID)
	r.mu.Unlock()
	return nil
}

// Nodes : nœuds lus à l'instant (back-office).
func (r *Registry) Nodes(ctx context.Context, clusterID string) ([]domain.Node, error) {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	reader, err := r.reader(c)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, domain.ErrClusterUnreachable
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	nodes, err := reader.Nodes(pctx)
	if errors.Is(err, domain.ErrAccessDenied) {
		return nil, domain.ErrAccessDenied
	}
	if err != nil {
		return nil, domain.ErrClusterUnreachable
	}
	return nodes, nil
}

// Check : relevé immédiat d'un cluster.
func (r *Registry) Check(ctx context.Context, clusterID string) (*domain.ClusterView, error) {
	c, err := r.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	r.probe(ctx, c, r.prevHealth(ctx, clusterID))
	return r.Get(ctx, clusterID)
}

// Run : relevés réguliers jusqu'à l'arrêt.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultHealthEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.CheckAll(ctx); err != nil && ctx.Err() == nil && r.Logger != nil {
			r.Logger.Warnw("cluster health pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckAll : un relevé par cluster en service, probeConcurrency à la fois.
func (r *Registry) CheckAll(ctx context.Context) error {
	cs, err := r.Store.List(ctx)
	if err != nil {
		return err
	}
	hs, err := r.Store.Healths(ctx)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i := range cs {
		c := &cs[i]
		if c.State == domain.StateDisabled {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.probe(ctx, c, hs[c.ClusterID])
		}()
	}
	wg.Wait()
	return nil
}

func (r *Registry) probe(ctx context.Context, c *domain.Cluster, prev *domain.Health) {
	now := r.now()
	reader, err := r.reader(c)
	if err == nil && reader == nil {
		r.save(ctx, c, prev, domain.NoAccessHealth(c, prev, now))
		return
	}
	var snap *domain.Snapshot
	start := time.Now()
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		snap, err = reader.Snapshot(pctx)
		cancel()
	}
	if ctx.Err() != nil {
		return // arrêt du process : pas un relevé
	}
	r.save(ctx, c, prev, domain.HealthFrom(c, prev, snap, err, time.Since(start), now))
}

func (r *Registry) save(ctx context.Context, c *domain.Cluster, prev, h *domain.Health) {
	if err := r.Store.SaveHealth(ctx, h); err != nil {
		if r.Logger != nil {
			r.Logger.Warnw("cluster health not saved", "cluster", c.ClusterID, "error", err)
		}
		return
	}
	if r.Observer != nil {
		r.Observer.ClusterHealth(ctx, c, h, prev == nil || prev.State != h.State)
	}
}

func (r *Registry) prevHealth(ctx context.Context, clusterID string) *domain.Health {
	hs, err := r.Store.Healths(ctx)
	if err != nil {
		return nil
	}
	return hs[clusterID]
}

// reader : client du cluster ; nil, nil : aucun accès (ni déposé ni configuré).
func (r *Registry) reader(c *domain.Cluster) (Reader, error) {
	if len(c.KubeconfigCiphertext) == 0 {
		if c.ClusterID == r.DefaultID && r.Default != nil {
			return r.Default, nil
		}
		return nil, nil
	}
	setAt := time.Time{}
	if c.KubeconfigSetAt != nil {
		setAt = *c.KubeconfigSetAt
	}
	r.mu.Lock()
	cached, ok := r.cache[c.ClusterID]
	r.mu.Unlock()
	if ok && cached.setAt.Equal(setAt) {
		return cached.reader, nil
	}
	if r.Sealer == nil {
		return nil, ErrNoSealer
	}
	raw, err := r.Sealer.Open(c.KubeconfigCiphertext, aad(c.ClusterID))
	if err != nil {
		return nil, err
	}
	reader, _, err := r.Dial(raw)
	if err != nil {
		return nil, err
	}
	r.remember(c.ClusterID, setAt, reader)
	return reader, nil
}

func (r *Registry) remember(clusterID string, setAt time.Time, reader Reader) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]cachedReader{}
	}
	r.cache[clusterID] = cachedReader{setAt: setAt, reader: reader}
}

func aad(clusterID string) string { return "cluster:" + clusterID + ":kubeconfig" }

// candidates : registre vu par le placement.
func (r *Registry) candidates(ctx context.Context) ([]domain.Candidate, error) {
	cs, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	hs, err := r.Store.Healths(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := r.Store.ProjectCounts(ctx, r.defaultID(cs))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Candidate, 0, len(cs))
	for i := range cs {
		c := &cs[i]
		out = append(out, domain.Candidate{ClusterID: c.ClusterID, Region: c.Region, State: c.State, IsDefault: c.IsDefault,
			Reachable: len(c.KubeconfigCiphertext) > 0 || (c.ClusterID == r.DefaultID && r.Default != nil),
			Health:    hs[c.ClusterID], Projects: counts[c.ClusterID]})
	}
	return out, nil
}

// Place : cluster d'un nouveau projet (région vide : au choix de la
// plateforme) ; "" : aucun cluster au registre (cluster de la configuration).
func (r *Registry) Place(ctx context.Context, region string) (string, error) {
	region = strings.TrimSpace(region)
	if region != "" && !domain.ValidRegionCode(region) {
		return "", domain.ErrInvalidRegionCode
	}
	cs, err := r.candidates(ctx)
	if err != nil {
		return "", err
	}
	return domain.Place(cs, region)
}

// Regions : régions proposées à la création d'un projet.
func (r *Registry) Regions(ctx context.Context) ([]domain.Region, error) {
	cs, err := r.candidates(ctx)
	if err != nil {
		return nil, err
	}
	return domain.Regions(cs), nil
}
