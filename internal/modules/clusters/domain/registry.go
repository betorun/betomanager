package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Registre des clusters (socle multi-cluster, lot 1) : l'équipe déclare
// chaque cluster (fournisseur, distribution, région), dépose son accès
// chiffré, et le worker relève sa santé.

// Fournisseurs de l'infrastructure qui porte le cluster.
const (
	ProviderOVH     = "ovh"
	ProviderProxmox = "proxmox"
	ProviderOther   = "other"
)

// Distributions Kubernetes.
const (
	DistributionK3s = "k3s"
	DistributionK8s = "k8s"
)

// États d'un cluster dans le registre.
const (
	// StateActive : reçoit de nouveaux projets (lot 2).
	StateActive = "active"
	// StateClosed : garde ses projets, n'en reçoit plus.
	StateClosed = "closed"
	// StateDisabled : hors service, plus sondé.
	StateDisabled = "disabled"
)

// États de santé.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthDown     = "down"
	HealthUnknown  = "unknown"
)

// Raisons d'une santé dégradée ou en panne (codes stables, traduits par la console).
const (
	ReasonNoAccess       = "no_access"       // aucun kubeconfig déposé
	ReasonAPIUnreachable = "api_unreachable" // l'API Kubernetes ne répond pas
	ReasonAccessDenied   = "access_denied"   // l'accès déposé n'a pas les droits de lecture (nœuds, pods)
	ReasonNoNodes        = "no_nodes"
	ReasonNodesNotReady  = "nodes_not_ready"
	ReasonWatcherMissing = "watcher_missing" // jamais enrôlé
	ReasonWatcherStale   = "watcher_stale"   // silencieux depuis WatcherStaleAfter
	ReasonCPUPressure    = "cpu_pressure"    // demandes ≥ PressureRatio de l'allouable
	ReasonMemoryPressure = "memory_pressure"
	ReasonPodsFailing    = "pods_failing"
)

const (
	// WatcherStaleAfter : watcher silencieux au-delà (il touche le registre à
	// chaque événement et à chaque relecture de sa configuration).
	WatcherStaleAfter = 10 * time.Minute
	// PressureRatio : part de l'allouable demandée au-delà de laquelle le
	// cluster est dit sous pression (85 %).
	PressureRatio = 0.85
	// KubeconfigMax : taille maximale d'un kubeconfig déposé.
	KubeconfigMax = 64 << 10
)

var (
	ErrClusterNotFound     = errors.New("cluster not found")
	ErrClusterExists       = errors.New("a cluster with this identifier already exists")
	ErrInvalidClusterID    = errors.New("invalid cluster identifier: 3 to 63 lowercase letters, digits or dashes")
	ErrInvalidProvider     = errors.New("invalid provider: ovh, proxmox or other")
	ErrInvalidDistribution = errors.New("invalid distribution: k3s or k8s")
	ErrInvalidState        = errors.New("invalid state: active, closed or disabled")
	ErrInvalidRegion       = errors.New("invalid region: up to 32 lowercase letters, digits or dashes")
	ErrNameTooLong         = errors.New("name too long (80 characters max)")
	ErrNotesTooLong        = errors.New("notes too long (2000 characters max)")
	ErrDefaultCluster      = errors.New("the default cluster cannot be deleted or disabled")
	ErrDisabledDefault     = errors.New("a disabled cluster cannot become the default")
	ErrClusterUnreachable  = errors.New("the cluster did not answer with this kubeconfig")
	// ErrAccessDenied : le cluster répond mais refuse la lecture des nœuds ou
	// des pods (droits du compte de l'accès).
	ErrAccessDenied = errors.New("the cluster refused to list nodes or pods with this kubeconfig")
)

var (
	clusterIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	regionRe    = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
)

// ValidClusterID : identifiant logique (celui du watcher).
func ValidClusterID(id string) bool { return clusterIDRe.MatchString(id) }

// ValidateRegistry : champs déclarés par l'équipe.
func ValidateRegistry(provider, distribution, region, state, name, notes string) error {
	switch {
	case provider != ProviderOVH && provider != ProviderProxmox && provider != ProviderOther:
		return ErrInvalidProvider
	case distribution != DistributionK3s && distribution != DistributionK8s:
		return ErrInvalidDistribution
	case state != StateActive && state != StateClosed && state != StateDisabled:
		return ErrInvalidState
	case !regionRe.MatchString(region):
		return ErrInvalidRegion
	case len([]rune(name)) > 80:
		return ErrNameTooLong
	case len([]rune(notes)) > 2000:
		return ErrNotesTooLong
	}
	return nil
}

// Health : dernier relevé de santé d'un cluster (table cluster_health).
type Health struct {
	ClusterID     string         `gorm:"primaryKey" json:"-"`
	State         string         `gorm:"not null" json:"state"`
	Reasons       pq.StringArray `gorm:"type:text[];not null;default:'{}'" json:"reasons"`
	Error         string         `gorm:"not null;default:''" json:"error,omitempty"`
	K8sVersion    string         `gorm:"column:k8s_version;not null;default:''" json:"k8s_version"`
	LatencyMs     int            `gorm:"not null;default:0" json:"latency_ms"`
	NodesTotal    int            `gorm:"not null;default:0" json:"nodes_total"`
	NodesReady    int            `gorm:"not null;default:0" json:"nodes_ready"`
	CPUAllocMilli int64          `gorm:"column:cpu_alloc_milli;not null;default:0" json:"cpu_alloc_milli"`
	CPUReqMilli   int64          `gorm:"column:cpu_req_milli;not null;default:0" json:"cpu_req_milli"`
	MemAllocBytes int64          `gorm:"not null;default:0" json:"mem_alloc_bytes"`
	MemReqBytes   int64          `gorm:"not null;default:0" json:"mem_req_bytes"`
	PodsRunning   int            `gorm:"not null;default:0" json:"pods_running"`
	PodsFailing   int            `gorm:"not null;default:0" json:"pods_failing"`
	CheckedAt     time.Time      `gorm:"not null" json:"checked_at"`
	Since         time.Time      `gorm:"not null" json:"since"`
}

func (Health) TableName() string { return "cluster_health" }

// ClusterView : un cluster du registre et sa santé (back-office).
type ClusterView struct {
	Cluster
	HasKubeconfig bool `json:"has_kubeconfig"`
	// FromConfig : cluster par défaut joint par la configuration du
	// déploiement (aucun kubeconfig déposé).
	FromConfig bool    `json:"from_config"`
	Health     *Health `json:"health,omitempty"`
}

// Node : un nœud (lecture à la demande, back-office).
type Node struct {
	Name          string   `json:"name"`
	Ready         bool     `json:"ready"`
	Unschedulable bool     `json:"unschedulable"`
	Roles         []string `json:"roles"`
	KubeletVer    string   `json:"kubelet_version"`
	CPUAllocMilli int64    `json:"cpu_alloc_milli"`
	MemAllocBytes int64    `json:"mem_alloc_bytes"`
	CPUReqMilli   int64    `json:"cpu_req_milli"`
	MemReqBytes   int64    `json:"mem_req_bytes"`
	Pods          int      `json:"pods"`
}

// Snapshot : ce que la sonde lit d'un cluster.
type Snapshot struct {
	Version     string
	Nodes       []Node
	PodsRunning int
	PodsFailing int
}

// Assess : état et raisons à partir d'un relevé ; err : l'API n'a pas répondu.
func Assess(c *Cluster, snap *Snapshot, err error, now time.Time) (string, []string) {
	reasons := watcherReasons(c, now)
	if errors.Is(err, ErrAccessDenied) {
		return HealthDown, append([]string{ReasonAccessDenied}, reasons...)
	}
	if err != nil || snap == nil {
		return HealthDown, append([]string{ReasonAPIUnreachable}, reasons...)
	}
	ready := 0
	var cpuA, cpuR, memA, memR int64
	for _, n := range snap.Nodes {
		if n.Ready {
			ready++
		}
		cpuA += n.CPUAllocMilli
		cpuR += n.CPUReqMilli
		memA += n.MemAllocBytes
		memR += n.MemReqBytes
	}
	switch {
	case len(snap.Nodes) == 0:
		return HealthDown, append([]string{ReasonNoNodes}, reasons...)
	case ready == 0:
		return HealthDown, append([]string{ReasonNodesNotReady}, reasons...)
	case ready < len(snap.Nodes):
		reasons = append(reasons, ReasonNodesNotReady)
	}
	if cpuA > 0 && float64(cpuR) >= PressureRatio*float64(cpuA) {
		reasons = append(reasons, ReasonCPUPressure)
	}
	if memA > 0 && float64(memR) >= PressureRatio*float64(memA) {
		reasons = append(reasons, ReasonMemoryPressure)
	}
	if snap.PodsFailing > 0 {
		reasons = append(reasons, ReasonPodsFailing)
	}
	if len(reasons) > 0 {
		return HealthDegraded, reasons
	}
	return HealthOK, reasons
}

// HealthFrom : relevé à enregistrer ; since gardé si l'état ne change pas.
func HealthFrom(c *Cluster, prev *Health, snap *Snapshot, err error, latency time.Duration, now time.Time) *Health {
	state, reasons := Assess(c, snap, err, now)
	h := &Health{ClusterID: c.ClusterID, State: state, Reasons: reasons, LatencyMs: int(latency.Milliseconds()), CheckedAt: now, Since: now}
	if prev != nil && prev.State == state {
		h.Since = prev.Since
	}
	if err != nil {
		h.Error = truncate(err.Error(), 300)
		return h
	}
	h.K8sVersion = snap.Version
	h.NodesTotal = len(snap.Nodes)
	for _, n := range snap.Nodes {
		if n.Ready {
			h.NodesReady++
		}
		h.CPUAllocMilli += n.CPUAllocMilli
		h.CPUReqMilli += n.CPUReqMilli
		h.MemAllocBytes += n.MemAllocBytes
		h.MemReqBytes += n.MemReqBytes
	}
	h.PodsRunning, h.PodsFailing = snap.PodsRunning, snap.PodsFailing
	return h
}

// watcherReasons : watcher jamais venu ou silencieux.
func watcherReasons(c *Cluster, now time.Time) []string {
	switch {
	case c.LastSeenAt == nil:
		return []string{ReasonWatcherMissing}
	case now.Sub(*c.LastSeenAt) > WatcherStaleAfter:
		return []string{ReasonWatcherStale}
	}
	return []string{}
}

// NoAccessHealth : cluster sans accès déposé (rien à sonder).
func NoAccessHealth(c *Cluster, prev *Health, now time.Time) *Health {
	h := &Health{ClusterID: c.ClusterID, State: HealthUnknown,
		Reasons: append([]string{ReasonNoAccess}, watcherReasons(c, now)...), CheckedAt: now, Since: now}
	if prev != nil && prev.State == HealthUnknown {
		h.Since = prev.Since
	}
	return h
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
