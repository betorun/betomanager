package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAssess(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-time.Hour)
	node := func(ready bool, cpuA, cpuR int64) Node {
		return Node{Ready: ready, CPUAllocMilli: cpuA, CPUReqMilli: cpuR, MemAllocBytes: 100, MemReqBytes: 10}
	}
	cases := []struct {
		name    string
		seen    *time.Time
		snap    *Snapshot
		err     error
		state   string
		reasons []string
	}{
		{"sain", &fresh, &Snapshot{Nodes: []Node{node(true, 1000, 100)}}, nil, HealthOK, []string{}},
		{"api muette", &fresh, nil, errors.New("timeout"), HealthDown, []string{ReasonAPIUnreachable}},
		{"droits manquants", &fresh, nil, fmt.Errorf("%w (nodes is forbidden)", ErrAccessDenied), HealthDown, []string{ReasonAccessDenied}},
		{"aucun nœud prêt", &fresh, &Snapshot{Nodes: []Node{node(false, 1000, 0)}}, nil, HealthDown, []string{ReasonNodesNotReady}},
		{"sans nœud", &fresh, &Snapshot{}, nil, HealthDown, []string{ReasonNoNodes}},
		{"un nœud sur deux", &fresh, &Snapshot{Nodes: []Node{node(true, 1000, 0), node(false, 1000, 0)}}, nil, HealthDegraded, []string{ReasonNodesNotReady}},
		{"CPU à 90 %", &fresh, &Snapshot{Nodes: []Node{node(true, 1000, 900)}}, nil, HealthDegraded, []string{ReasonCPUPressure}},
		{"watcher silencieux", &stale, &Snapshot{Nodes: []Node{node(true, 1000, 0)}}, nil, HealthDegraded, []string{ReasonWatcherStale}},
		{"watcher jamais venu", nil, &Snapshot{Nodes: []Node{node(true, 1000, 0)}, PodsFailing: 2}, nil, HealthDegraded, []string{ReasonWatcherMissing, ReasonPodsFailing}},
	}
	for _, c := range cases {
		state, reasons := Assess(&Cluster{LastSeenAt: c.seen}, c.snap, c.err, now)
		assert.Equal(t, c.state, state, c.name)
		assert.Equal(t, c.reasons, reasons, c.name)
	}
}

// « Depuis » ne bouge pas tant que l'état ne change pas.
func TestHealthFrom_KeepsSince(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	seen := now
	c := &Cluster{ClusterID: "k", LastSeenAt: &seen}
	snap := &Snapshot{Version: "v1.31", Nodes: []Node{{Ready: true, CPUAllocMilli: 4000, CPUReqMilli: 1000, MemAllocBytes: 8, MemReqBytes: 2}}, PodsRunning: 3}
	first := HealthFrom(c, nil, snap, nil, 30*time.Millisecond, now.Add(-time.Hour))
	again := HealthFrom(c, first, snap, nil, 0, now)
	assert.Equal(t, first.Since, again.Since)
	assert.Equal(t, int64(4000), again.CPUAllocMilli)
	assert.Equal(t, 1, again.NodesReady)
	down := HealthFrom(c, again, nil, errors.New("refused"), 0, now)
	assert.Equal(t, now, down.Since)
	assert.Equal(t, "refused", down.Error)
}

func TestValidateRegistry(t *testing.T) {
	assert.NoError(t, ValidateRegistry(ProviderOVH, DistributionK3s, "eu-west-gra", StateActive, "Prod", ""))
	assert.ErrorIs(t, ValidateRegistry("aws", DistributionK3s, "", StateActive, "", ""), ErrInvalidProvider)
	assert.ErrorIs(t, ValidateRegistry(ProviderOVH, "openshift", "", StateActive, "", ""), ErrInvalidDistribution)
	assert.ErrorIs(t, ValidateRegistry(ProviderOVH, DistributionK8s, "EU West", StateActive, "", ""), ErrInvalidRegion)
	assert.ErrorIs(t, ValidateRegistry(ProviderOVH, DistributionK8s, "", "paused", "", ""), ErrInvalidState)
	assert.True(t, ValidClusterID("prod-fr-gra-1"))
	assert.False(t, ValidClusterID("Prod_1"))
}
