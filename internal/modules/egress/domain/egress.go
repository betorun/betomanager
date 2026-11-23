// Package domain : IP de sortie dédiée par projet (ADR 0014). Rien ici ne
// dépend du système qui fait sortir le trafic par l'IP (Cilium aujourd'hui).
package domain

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Passerelles de sortie connues d'un cluster ("" : aucune).
const (
	GatewayNone   = ""
	GatewayCilium = "cilium"
)

var (
	ErrAddressNotFound   = errors.New("egress address not found")
	ErrAddressExists     = errors.New("this egress address is already declared")
	ErrInvalidAddress    = errors.New("invalid egress address: a public IPv4 address")
	ErrInvalidNode       = errors.New("invalid gateway node name")
	ErrAddressInUse      = errors.New("this egress address is assigned to a project: release it first")
	ErrNoGateway         = errors.New("dedicated egress addresses are not available on this project's region")
	ErrNoFreeAddress     = errors.New("no dedicated egress address is free in this project's region for now")
	ErrAlreadyHasAddress = errors.New("this project already has a dedicated egress address")
	ErrNoAddress         = errors.New("this project has no dedicated egress address")
	ErrNotesTooLong      = errors.New("notes too long (500 characters max)")
	ErrProjectNotFound   = errors.New("project not found")
)

// nodeRe : nom de nœud Kubernetes (sous-domaine DNS).
var nodeRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// NormalizeAddress : IPv4 publique, forme canonique ; sinon "".
func NormalizeAddress(raw string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() || isShared(a) {
		return ""
	}
	return a.String()
}

// isShared : 100.64.0.0/10 (CGNAT), jamais routable depuis Internet.
func isShared(a netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// ValidNode : nom de nœud passerelle.
func ValidNode(n string) bool { return nodeRe.MatchString(n) }

// Address : une IP de la réserve d'un cluster.
type Address struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	ClusterID   string     `gorm:"not null" json:"cluster_id"`
	Address     string     `gorm:"not null" json:"address"`
	GatewayNode string     `gorm:"not null" json:"gateway_node"`
	ProjectID   *uuid.UUID `gorm:"type:uuid" json:"-"`
	AssignedAt  *time.Time `json:"assigned_at,omitempty"`
	AssignedBy  string     `gorm:"not null;default:''" json:"-"`
	Notes       string     `gorm:"not null;default:''" json:"notes"`
	CreatedBy   string     `gorm:"not null;default:''" json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (Address) TableName() string { return "egress_addresses" }

// AdminAddress : vue de l'équipe (projet nommé, jamais l'identifiant).
type AdminAddress struct {
	Address
	ProjectName string `json:"project_name,omitempty"`
	Assigned    bool   `json:"assigned"`
}

// Binding : règle à poser sur le cluster (projet → IP par la passerelle).
type Binding struct {
	ClusterID   string
	Namespace   string
	Address     string
	GatewayNode string
}

// ProjectEgress : sortie d'un projet (vue client).
type ProjectEgress struct {
	// Address : IP dédiée du projet ("" : aucune ; le trafic sort par une
	// adresse partagée de la plateforme, non garantie).
	Address string `json:"address,omitempty"`
	// Available : une IP dédiée peut être demandée maintenant.
	Available bool `json:"available"`
	// Offered : la région du projet propose des IP dédiées (même si aucune
	// n'est libre pour l'instant).
	Offered    bool       `json:"offered"`
	AssignedAt *time.Time `json:"assigned_at,omitempty"`
}
