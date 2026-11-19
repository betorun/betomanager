package sites

import (
	"github.com/gin-gonic/gin"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"github.com/thekrauss/kubemanager/internal/edge"
)

// EdgeController : questions de static-edge, qui tourne dans le cluster et ne
// joint la plateforme que par l'API en HTTPS (pas d'accès à Postgres).
type EdgeController struct {
	Resolver edge.Resolver
}

type EdgeResolveRequest struct {
	ClusterID string `path:"clusterID" description:"Cluster de l'edge (sa clé ne vaut que pour lui)"`
	Host      string `query:"host" binding:"required,max=253" description:"Nom d'hôte demandé par le visiteur"`
}

// EdgeSite : réponse de Resolve ; mêmes champs que edge.Site.
type EdgeSite struct {
	SiteID       string `json:"site_id"`
	DeploymentID string `json:"deployment_id"`
	SPAFallback  bool   `json:"spa_fallback"`
	// Suspended : organisation suspendue, l'edge sert une page neutre (503).
	Suspended bool `json:"suspended,omitempty"`
}

// Resolve : site servi pour un nom d'hôte (sites LIVE avec un déploiement
// actif, domaines vérifiés) ; 404 sinon.
func (h *EdgeController) Resolve(c *gin.Context, in *EdgeResolveRequest) (*EdgeSite, error) {
	s, err := h.Resolver.Resolve(c.Request.Context(), edge.NormalizeHost(in.Host))
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "no site for this host")
	}
	return &EdgeSite{SiteID: s.ID, DeploymentID: s.DeploymentID, SPAFallback: s.SPAFallback, Suspended: s.Suspended}, nil
}
