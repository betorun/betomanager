package clusters

import (
	"strings"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware/security"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/service"

	"github.com/gin-gonic/gin"
)

type IClusterController interface {
	Enroll(c *gin.Context, in *domain.EnrollRequest) (*domain.EnrollResponse, error)
	GetConfig(c *gin.Context, in *ClusterIDPath) (*domain.RemoteConfigResponse, error)
	OnPodEvent(c *gin.Context, in *PodEventInput) (*domain.PodEventAck, error)
	// Admin : liste les clusters enrollés avec leur LastSeenAt (monitoring staleness).
	ListClusters(c *gin.Context) ([]domain.Cluster, error)
}

type Controller struct {
	service service.IClusterService
	// Registry : registre et santé (nil : routes du registre non montées).
	Registry *service.Registry
	// Agents : installation des agents (nil : routes non montées).
	Agents *service.Agents
}

func NewController(s service.IClusterService) *Controller {
	return &Controller{service: s}
}

var _ IClusterController = (*Controller)(nil)

// ── DTOs path params ──────────────────────────────────────────────────────

type ClusterIDPath struct {
	ClusterID string `path:"clusterID" binding:"required"`
}

// PodEventInput : path param + body JSON. Corps embarqué : encoding/json
// ignore « inline », un champ Event nommé restait vide (événements perdus).
type PodEventInput struct {
	ClusterID string `path:"clusterID" binding:"required"`
	domain.PodEvent
}

// ── Handlers ──────────────────────────────────────────────────────────────

func (ctrl *Controller) Enroll(c *gin.Context, in *domain.EnrollRequest) (*domain.EnrollResponse, error) {
	if in == nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "body required")
	}
	return ctrl.service.Enroll(c.Request.Context(), *in)
}

func (ctrl *Controller) GetConfig(c *gin.Context, in *ClusterIDPath) (*domain.RemoteConfigResponse, error) {
	return ctrl.service.GetConfig(c.Request.Context(), in.ClusterID)
}

func (ctrl *Controller) OnPodEvent(c *gin.Context, in *PodEventInput) (*domain.PodEventAck, error) {
	if in == nil {
		return nil, betoerrors.New(betoerrors.CodeInvalidInput, "body required")
	}
	return ctrl.service.OnPodEvent(c.Request.Context(), in.ClusterID, in.PodEvent)
}

func (ctrl *Controller) ListClusters(c *gin.Context) ([]domain.Cluster, error) {
	return ctrl.service.ListClusters(c.Request.Context())
}

// ── Registre (back-office) ────────────────────────────────────────────────

// ClusterPath : cluster visé.
type ClusterPath struct {
	ClusterID string `path:"clusterID" binding:"required" description:"Identifiant du cluster"`
}

// UpdateClusterInput : chemin et champs modifiés.
type UpdateClusterInput struct {
	ClusterID string `path:"clusterID" binding:"required"`
	service.UpdateInput
}

// KubeconfigInput : accès du cluster (jamais rendu ni journalisé).
type KubeconfigInput struct {
	ClusterID  string `path:"clusterID" binding:"required"`
	Kubeconfig string `json:"kubeconfig" binding:"required" description:"Kubeconfig complet (YAML) : jeton ou certificat inclus, sans commande exec ni fichier local, serveur en https"`
}

// BootstrapClusterInput : préparation d'un cluster par un administrateur.
type BootstrapClusterInput struct {
	ClusterID  string `path:"clusterID" binding:"required"`
	Kubeconfig string `json:"kubeconfig" binding:"required" description:"Kubeconfig administrateur (YAML), utilisé le temps de l'installation et jamais enregistré"`
	Image      string `json:"image,omitempty" description:"Image de l'agent (vide : celle de la configuration)"`
}

// AgentUpdateInput : mise à jour de l'agent.
type AgentUpdateInput struct {
	ClusterID string `path:"clusterID" binding:"required"`
	service.AgentInput
}

func principal(c *gin.Context) string {
	if p, ok := security.PrincipalFromContext(c); ok {
		return string(p)
	}
	return ""
}

// Registry : registre et derniers relevés.
func (ctrl *Controller) RegistryList(c *gin.Context) ([]domain.ClusterView, error) {
	return ctrl.Registry.List(c.Request.Context())
}

func (ctrl *Controller) RegistryGet(c *gin.Context, in *ClusterPath) (*domain.ClusterView, error) {
	return ctrl.Registry.Get(c.Request.Context(), in.ClusterID)
}

func (ctrl *Controller) RegistryDeclare(c *gin.Context, in *service.DeclareInput) (*domain.ClusterView, error) {
	return ctrl.Registry.Declare(c.Request.Context(), *in, principal(c))
}

func (ctrl *Controller) RegistryUpdate(c *gin.Context, in *UpdateClusterInput) (*domain.ClusterView, error) {
	return ctrl.Registry.Update(c.Request.Context(), in.ClusterID, in.UpdateInput)
}

func (ctrl *Controller) RegistrySetKubeconfig(c *gin.Context, in *KubeconfigInput) (*domain.ClusterView, error) {
	raw := strings.TrimSpace(in.Kubeconfig)
	return ctrl.Registry.SetKubeconfig(c.Request.Context(), in.ClusterID, []byte(raw), principal(c))
}

func (ctrl *Controller) RegistrySetDefault(c *gin.Context, in *ClusterPath) (*domain.ClusterView, error) {
	return ctrl.Registry.SetDefault(c.Request.Context(), in.ClusterID)
}

func (ctrl *Controller) RegistryCheck(c *gin.Context, in *ClusterPath) (*domain.ClusterView, error) {
	return ctrl.Registry.Check(c.Request.Context(), in.ClusterID)
}

func (ctrl *Controller) RegistryNodes(c *gin.Context, in *ClusterPath) ([]domain.Node, error) {
	return ctrl.Registry.Nodes(c.Request.Context(), in.ClusterID)
}

func (ctrl *Controller) RegistryDelete(c *gin.Context, in *ClusterPath) error {
	return ctrl.Registry.Delete(c.Request.Context(), in.ClusterID)
}

// AgentBootstrap : compte plateforme et agent posés avec un accès administrateur.
func (ctrl *Controller) AgentBootstrap(c *gin.Context, in *BootstrapClusterInput) (*domain.AgentState, error) {
	raw := strings.TrimSpace(in.Kubeconfig)
	return ctrl.Agents.Bootstrap(c.Request.Context(), in.ClusterID, service.BootstrapInput{Kubeconfig: []byte(raw), Image: in.Image}, principal(c))
}

// AgentUpdate : image ou clé de l'agent renouvelée par le compte plateforme.
func (ctrl *Controller) AgentUpdate(c *gin.Context, in *AgentUpdateInput) (*domain.AgentState, error) {
	return ctrl.Agents.Update(c.Request.Context(), in.ClusterID, in.AgentInput, principal(c))
}

// AgentState : agent lu sur le cluster.
func (ctrl *Controller) AgentState(c *gin.Context, in *ClusterPath) (*domain.AgentState, error) {
	return ctrl.Agents.State(c.Request.Context(), in.ClusterID)
}

// Regions : régions proposées à la création d'un projet (clients).
func (ctrl *Controller) Regions(c *gin.Context) ([]domain.Region, error) {
	return ctrl.Registry.Regions(c.Request.Context())
}
