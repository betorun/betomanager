package service

import (
	"context"

	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/infrastructure/observability"
	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// MetricsObserver : santé exposée à Prometheus (alertes ClusterDown,
// ClusterDegraded) et changements d'état journalisés.
type MetricsObserver struct{ Logger *zap.SugaredLogger }

var healthValue = map[string]float64{domain.HealthOK: 0, domain.HealthDegraded: 1, domain.HealthDown: 2, domain.HealthUnknown: 3}

func (o MetricsObserver) ClusterHealth(_ context.Context, c *domain.Cluster, h *domain.Health, changed bool) {
	observability.ClusterHealthState.WithLabelValues(c.ClusterID, c.Provider, c.Region).Set(healthValue[h.State])
	if changed && o.Logger != nil {
		o.Logger.Infow("cluster health changed", "cluster", c.ClusterID, "state", h.State, "reasons", []string(h.Reasons))
	}
}
