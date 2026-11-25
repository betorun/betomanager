package clusters

import (
	"context"
	"strings"

	"github.com/google/uuid"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
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
	// Access, OrgCountry : région suggérée selon le pays d'une organisation
	// que l'appelant peut voir (nil : aucune suggestion).
	Access     OrgAccess
	OrgCountry func(ctx context.Context, orgID uuid.UUID) (string, error)
}

// OrgAccess : droit de voir une organisation (iam.Engine).
type OrgAccess interface {
	Check(ctx context.Context, p iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error)
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

// RegionsQuery : organisation dont le pays désigne la région suggérée.
type RegionsQuery struct {
	OrganizationID string `query:"organization_id" description:"Organisation du futur projet (facultatif) : la région associée à son pays est suggérée et passe en tête"`
}

// Regions : régions proposées à la création d'un projet (clients).
func (ctrl *Controller) Regions(c *gin.Context, in *RegionsQuery) ([]domain.Region, error) {
	cc, err := ctrl.suggestionCountry(c, in.OrganizationID)
	if err != nil {
		return nil, err
	}
	return ctrl.Registry.Regions(c.Request.Context(), cc)
}

// ProbeOriginsResponse : origines des adresses de mesure des régions.
type ProbeOriginsResponse struct {
	Origins []string `json:"origins" description:"Origines (https://hôte) des adresses de mesure du temps de réponse des régions"`
}

// ProbeOrigins : origines des adresses de mesure (public : la console les lit
// côté serveur pour sa politique de contenu, avant toute connexion).
func (ctrl *Controller) ProbeOrigins(c *gin.Context) (*ProbeOriginsResponse, error) {
	o, err := ctrl.Registry.ProbeOrigins(c.Request.Context())
	if err != nil {
		return nil, err
	}
	c.Header("Cache-Control", "public, max-age=300")
	return &ProbeOriginsResponse{Origins: o}, nil
}

// suggestionCountry : pays de l'organisation, si l'appelant peut la voir ;
// sinon "" (aucune suggestion, rien n'est révélé de l'organisation).
func (ctrl *Controller) suggestionCountry(c *gin.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || ctrl.Access == nil || ctrl.OrgCountry == nil {
		return "", nil
	}
	orgID, err := uuid.Parse(raw)
	if err != nil {
		return "", betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
	}
	p, ok := security.PrincipalFromContext(c)
	if !ok {
		return "", nil
	}
	ctx := c.Request.Context()
	dec, err := ctrl.Access.Check(ctx, p, iam.OrganizationsGet, iam.OrganizationName(orgID))
	if err != nil || !dec.Allowed {
		return "", nil
	}
	return ctrl.OrgCountry(ctx, orgID)
}

// ── Pays → région et adresses de mesure (back-office) ─────────────────────

// CountryPath : pays visé.
type CountryPath struct {
	Country string `path:"country" binding:"required" description:"Pays (code ISO 3166-1 à deux lettres, ex. GA)"`
}

// SetCountryRegionInput : région suggérée aux clients du pays.
type SetCountryRegionInput struct {
	Country string `path:"country" binding:"required" description:"Pays (code ISO 3166-1 à deux lettres, ex. GA)"`
	Region  string `json:"region" binding:"required" description:"Code d'une région portée par au moins un cluster enregistré"`
}

// RegionCodePath : région visée.
type RegionCodePath struct {
	Code string `path:"code" binding:"required" description:"Code de la région"`
}

// SetRegionProbeInput : adresse de mesure de la région.
type SetRegionProbeInput struct {
	Code     string `path:"code" binding:"required" description:"Code de la région"`
	ProbeURL string `json:"probe_url" description:"Adresse https qui répond vite et sans données (ex. route /healthz publique d'un point d'entrée de la région) ; vide : retirée"`
}

func (ctrl *Controller) CountryRegions(c *gin.Context) ([]domain.CountryRegion, error) {
	return ctrl.Registry.CountryRegions(c.Request.Context())
}

func (ctrl *Controller) SetCountryRegion(c *gin.Context, in *SetCountryRegionInput) (*domain.CountryRegion, error) {
	e, err := ctrl.Registry.SetCountryRegion(c.Request.Context(), in.Country, in.Region, principal(c))
	if err == nil {
		audit.Annotate(c.Request.Context(), "country", e.Country)
		audit.Annotate(c.Request.Context(), "region", e.Region)
	}
	return e, err
}

func (ctrl *Controller) DeleteCountryRegion(c *gin.Context, in *CountryPath) error {
	return ctrl.Registry.DeleteCountryRegion(c.Request.Context(), in.Country)
}

func (ctrl *Controller) RegionSettings(c *gin.Context) ([]domain.RegionSetting, error) {
	return ctrl.Registry.RegionSettings(c.Request.Context())
}

func (ctrl *Controller) SetRegionProbe(c *gin.Context, in *SetRegionProbeInput) (*domain.RegionSetting, error) {
	e, err := ctrl.Registry.SetRegionProbe(c.Request.Context(), in.Code, in.ProbeURL, principal(c))
	if err == nil {
		audit.Annotate(c.Request.Context(), "probe_url", e.ProbeURL)
	}
	return e, err
}
