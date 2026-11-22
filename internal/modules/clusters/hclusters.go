package clusters

import (
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

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
