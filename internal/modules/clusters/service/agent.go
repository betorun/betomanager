package service

import (
	"context"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// Agent d'un cluster (lot 3) : préparation par un administrateur, une fois,
// puis renouvellement par le compte plateforme. Voir domain/agent.go.

// AgentInstaller : objets posés sur un cluster (repository.Installer).
type AgentInstaller interface {
	Platform(ctx context.Context) (token, ca []byte, err error)
	AgentRBAC(ctx context.Context) error
	Agent(ctx context.Context, spec domain.AgentSpec) error
	State(ctx context.Context) (*domain.AgentState, error)
}

// AgentKeys : fichiers de clé du compte système du cluster (iam.ClusterAgents).
type AgentKeys interface {
	Issue(ctx context.Context, by, clusterID string) (keyID string, keyFile []byte, err error)
	Revoke(ctx context.Context, keyID string) error
}

// Agents : installation et suivi des agents.
type Agents struct {
	Registry *Registry
	Keys     AgentKeys // nil : installation impossible (pas de STS)
	// Image : image par défaut de l'agent (kubernetes.agent_image).
	Image string
	// APIURL : …/kmanager/v1/clusters vu depuis les clusters.
	APIURL string
	// Prepare : installateur d'un kubeconfig de préparation (contrôlé comme
	// un dépôt), et l'adresse de son serveur. Jamais enregistré.
	Prepare func(raw []byte) (AgentInstaller, string, error)
	// For : installateur par l'accès enregistré du cluster (compte plateforme).
	For func(ctx context.Context, clusterID string) (AgentInstaller, error)
	// RestrictedKubeconfig : kubeconfig du compte plateforme.
	RestrictedKubeconfig func(clusterID, server string, token, ca []byte) ([]byte, error)
	Now                  func() time.Time
}

// BootstrapInput : préparation d'un cluster.
type BootstrapInput struct {
	// Kubeconfig : administrateur, utilisé le temps de l'appel, jamais enregistré.
	Kubeconfig []byte
	Image      string
}

// AgentInput : mise à jour de l'agent par le compte plateforme.
type AgentInput struct {
	Image     string `json:"image,omitempty" description:"Image de l'agent (vide : celle en place, sinon celle de la configuration)"`
	RotateKey bool   `json:"rotate_key,omitempty" description:"Nouveau fichier de clé ; l'ancien est révoqué une fois l'agent mis à jour"`
}

func (a *Agents) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agents) image(requested, current string) (string, error) {
	img := strings.TrimSpace(requested)
	if img == "" {
		img = current
	}
	if img == "" {
		img = strings.TrimSpace(a.Image)
	}
	if img == "" {
		return "", domain.ErrAgentImageMissing
	}
	if !domain.ValidAgentImage(img) {
		return "", domain.ErrInvalidAgentImage
	}
	return img, nil
}

func (a *Agents) ready(c *domain.Cluster) error {
	switch {
	case a.Keys == nil:
		return domain.ErrAgentKeysUnavailable
	case strings.TrimSpace(a.APIURL) == "":
		return domain.ErrAgentAPIURLMissing
	case c.State == domain.StateDisabled:
		return domain.ErrClusterDisabled
	}
	return nil
}

// Bootstrap : compte plateforme, agent et sa clé posés avec un kubeconfig
// administrateur ; seul le kubeconfig restreint est enregistré.
func (a *Agents) Bootstrap(ctx context.Context, clusterID string, in BootstrapInput, by string) (*domain.AgentState, error) {
	c, err := a.Registry.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if err := a.ready(c); err != nil {
		return nil, err
	}
	if a.Registry.Sealer == nil {
		return nil, ErrNoSealer
	}
	img, err := a.image(in.Image, "")
	if err != nil {
		return nil, err
	}
	inst, server, err := a.Prepare(in.Kubeconfig)
	if err != nil {
		return nil, err
	}
	token, ca, err := inst.Platform(ctx)
	if err != nil {
		return nil, err
	}
	if err := inst.AgentRBAC(ctx); err != nil {
		return nil, err
	}
	keyID, err := a.install(ctx, inst, c, img, true, by)
	if err != nil {
		return nil, err
	}
	// Agent enregistré d'abord : sa clé est suivie même si la suite échoue.
	st, err := a.finish(ctx, c, inst, img, keyID, by)
	if err != nil {
		return nil, err
	}
	restricted, err := a.RestrictedKubeconfig(clusterID, server, token, ca)
	if err != nil {
		return nil, err
	}
	// Contrôle le compte restreint (lecture des nœuds et des pods) avant de le garder.
	if _, err := a.Registry.SetKubeconfig(ctx, clusterID, restricted, by); err != nil {
		return nil, err
	}
	return st, nil
}

// Update : nouvelle image ou nouvelle clé, par le compte plateforme.
func (a *Agents) Update(ctx context.Context, clusterID string, in AgentInput, by string) (*domain.AgentState, error) {
	c, err := a.Registry.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if err := a.ready(c); err != nil {
		return nil, err
	}
	img, err := a.image(in.Image, c.AgentImage)
	if err != nil {
		return nil, err
	}
	inst, err := a.For(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	// Jamais installé par la plateforme : il faut une clé.
	rotate := in.RotateKey || c.AgentKeyID == ""
	keyID, err := a.install(ctx, inst, c, img, rotate, by)
	if err != nil {
		return nil, err
	}
	if keyID == "" {
		keyID = c.AgentKeyID
	}
	return a.finish(ctx, c, inst, img, keyID, by)
}

// State : agent lu par le compte plateforme, avec son dernier signe de vie.
func (a *Agents) State(ctx context.Context, clusterID string) (*domain.AgentState, error) {
	c, err := a.Registry.Store.Get(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	inst, err := a.For(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	st, err := inst.State(ctx)
	if err != nil {
		return nil, err
	}
	st.LastSeenAt, st.InstalledAt = c.LastSeenAt, c.AgentInstalledAt
	return st, nil
}

// install : nouvelle clé au besoin puis agent ; clé révoquée si l'agent n'a
// pas pu être posé. Rend la nouvelle clé ("" : clé gardée).
func (a *Agents) install(ctx context.Context, inst AgentInstaller, c *domain.Cluster, img string, newKey bool, by string) (string, error) {
	spec := domain.AgentSpec{ClusterID: c.ClusterID, Image: img, APIURL: strings.TrimSpace(a.APIURL)}
	var keyID string
	if newKey {
		id, file, err := a.Keys.Issue(ctx, by, c.ClusterID)
		if err != nil {
			return "", err
		}
		keyID, spec.KeyFile = id, file
	}
	if err := inst.Agent(ctx, spec); err != nil {
		if keyID != "" {
			a.revoke(keyID)
		}
		return "", err
	}
	return keyID, nil
}

// finish : agent enregistré, ancienne clé révoquée.
func (a *Agents) finish(ctx context.Context, c *domain.Cluster, inst AgentInstaller, img, keyID, by string) (*domain.AgentState, error) {
	now := a.now()
	if err := a.Registry.Store.Update(ctx, c.ClusterID, map[string]any{"agent_image": img, "agent_key_id": keyID,
		"agent_installed_at": now, "agent_installed_by": by}); err != nil {
		return nil, err
	}
	if c.AgentKeyID != "" && c.AgentKeyID != keyID {
		a.revoke(c.AgentKeyID)
	}
	st, err := inst.State(ctx)
	if err != nil {
		// Posé, mais l'état n'a pas pu être relu : il le sera à l'affichage.
		st = &domain.AgentState{Installed: true, Image: img, ManagedByPlatform: true}
	}
	st.LastSeenAt, st.InstalledAt = c.LastSeenAt, &now
	return st, nil
}

// revoke : au mieux, la requête a déjà réussi ou échoué pour une autre raison.
func (a *Agents) revoke(keyID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Keys.Revoke(ctx, keyID); err != nil && a.Registry.Logger != nil {
		a.Registry.Logger.Warnw("cluster agent key not revoked", "key", keyID, "error", err)
	}
}
