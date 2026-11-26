// Package domain : VPC d'un projet (ADR 0019), décrit sans rien de
// l'hébergeur ni de l'outil qui le met en œuvre. Le produit ne parle que de
// réseau privé, de membres, d'adresses, de noms et de règles ; la mise en
// œuvre passe par VPCBackend et FirewallProvider (tunnel chez un fournisseur
// sans réseau privé, réseau natif sous Proxmox…), jamais nommée au client.
package domain

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// États d'un rattachement.
const (
	// StatePending : prévu côté plateforme, le VPS ne s'est pas encore relié
	// (inscription attendue, ou réseau en préparation).
	StatePending = "pending"
	// StateAttached : le VPS est relié au réseau privé.
	StateAttached = "attached"
)

// NameSuffix : suffixe des noms du réseau privé (db-main-rw.internal).
const NameSuffix = ".internal"

// EnrollmentTTL : durée de vie d'un jeton d'inscription (usage unique).
const EnrollmentTTL = 30 * time.Minute

// Network : VPC d'un projet (un par projet, créé au premier rattachement).
type Network struct {
	ProjectID uuid.UUID `gorm:"type:uuid;primaryKey" json:"project_id"`
	ClusterID string    `gorm:"not null;default:''" json:"-"`
	CIDR      string    `gorm:"column:cidr;type:cidr;not null" json:"cidr"`
	CreatedAt time.Time `json:"created_at"`
}

func (Network) TableName() string { return "vpc_networks" }

// Attachment : un VPS rattaché au VPC de son projet.
type Attachment struct {
	VPSID     uuid.UUID `gorm:"column:vps_id;type:uuid;primaryKey" json:"vps_id"`
	ProjectID uuid.UUID `gorm:"type:uuid;not null" json:"project_id"`
	Address   string    `gorm:"type:inet;not null" json:"address"`
	// Backend : mise en œuvre retenue ; interne, jamais rendue par l'API.
	Backend       string     `gorm:"not null" json:"-"`
	State         string     `gorm:"not null" json:"state"`
	PublicKey     string     `gorm:"not null;default:''" json:"-"`
	CreatedBy     string     `gorm:"not null;default:''" json:"-"`
	CreatedAt     time.Time  `json:"created_at"`
	AttachedAt    *time.Time `json:"attached_at,omitempty"`
	LastContactAt *time.Time `json:"last_contact_at,omitempty"`
}

func (Attachment) TableName() string { return "vpc_attachments" }

// Enrollment : jeton d'inscription d'un VPS (seule son empreinte est gardée).
type Enrollment struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	VPSID     uuid.UUID `gorm:"column:vps_id;type:uuid;not null"`
	TokenHash []byte    `gorm:"not null"`
	ExpiresAt time.Time `gorm:"not null"`
	UsedAt    *time.Time
	CreatedBy string `gorm:"not null;default:''"`
	CreatedAt time.Time
}

func (Enrollment) TableName() string { return "vpc_enrollments" }

// ── Mise en œuvre ─────────────────────────────────────────────────────

// Placement : où vivent le VPS et le réseau du projet ; sert à choisir la
// mise en œuvre (VPCBackend.Handles). Jamais montré au client.
type Placement struct {
	// VPSProvider : hébergeur du VPS (compute_vps.provider).
	VPSProvider string
	// ClusterID, ClusterProvider : cluster du projet et son hébergeur
	// (clusters.provider : ovh, proxmox, other).
	ClusterID       string
	ClusterProvider string
}

// VPC : ce qu'une mise en œuvre reçoit d'un réseau.
type VPC struct {
	ProjectID uuid.UUID
	ClusterID string
	CIDR      netip.Prefix
	// Gateway : première adresse de la plage, réservée à la plateforme.
	Gateway netip.Addr
}

// Member : un VPS du réseau, vu par la mise en œuvre.
type Member struct {
	VPSID     uuid.UUID
	Name      string
	Address   netip.Addr
	PublicKey string // vide : pas encore inscrit (selon la mise en œuvre)
	// PublicIPv4 : adresse publique du VPS (entrée du réseau privé, pare-feu).
	PublicIPv4 string
}

// Capabilities : ce que la mise en œuvre sait faire. La console et l'API les
// lisent au lieu de chiffres en dur.
type Capabilities struct {
	// NeedsEnrollment : le VPS doit s'inscrire lui-même (jeton, commande à
	// lancer) ; faux pour un réseau natif où le VPS naît relié.
	NeedsEnrollment bool `json:"needs_enrollment"`
	// IPv6 : adresses privées IPv6 possibles.
	IPv6 bool `json:"ipv6"`
}

// AttachPlan : ce que le VPS doit appliquer pour se relier.
type AttachPlan struct {
	// NeedsEnrollment : une inscription par le VPS est attendue.
	NeedsEnrollment bool
}

// EnrollRequest : inscription envoyée par le VPS (clé publique générée sur lui).
type EnrollRequest struct {
	VPC       VPC
	Member    Member
	PublicKey string
}

// EnrollResult : configuration rendue au VPS, sans aucun secret de la plateforme.
type EnrollResult struct {
	// Config : paramètres propres à la mise en œuvre (adresse du point
	// d'entrée, clé publique de la plateforme…), appliqués par le script
	// d'inscription ; jamais affichés dans la console.
	Config map[string]string
}

// MemberStatus : état d'un membre vu par le réseau.
type MemberStatus struct {
	VPSID       uuid.UUID
	Connected   bool
	LastContact *time.Time
	// Traffic : compteurs du lien depuis sa mise en place côté réseau (nil :
	// non mesuré). Ils repartent de zéro quand le lien est refait.
	Traffic *Traffic
}

// Traffic : octets passés par le lien d'un VPS (FromVPS : envoyés par le VPS
// vers le projet ; ToVPS : reçus par le VPS).
type Traffic struct {
	FromVPS int64
	ToVPS   int64
}

// ProjectTraffic : trafic du réseau privé d'un projet sur un mois (relevé
// pour l'équipe ; rien n'est facturé).
type ProjectTraffic struct {
	ProjectID        uuid.UUID `json:"project_id"`
	ProjectName      string    `json:"project_name"`
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	FromVPSBytes     int64     `json:"from_vps_bytes"`
	ToVPSBytes       int64     `json:"to_vps_bytes"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// VPCBackend : relie les VPS d'un projet à son réseau privé. Chaque mise en
// œuvre passe vpctest.RunBackendContract.
type VPCBackend interface {
	// Name : identifiant stable, jamais montré au client.
	Name() string
	// Handles : cette mise en œuvre sait relier un VPS ainsi placé.
	Handles(p Placement) bool
	Capabilities() Capabilities
	// Ensure : le réseau existe et connaît ces membres (idempotent).
	Ensure(ctx context.Context, vpc VPC, members []Member) error
	// Attach : prépare l'entrée d'un VPS (idempotent).
	Attach(ctx context.Context, vpc VPC, m Member) (*AttachPlan, error)
	// Enroll : le VPS s'inscrit (si la mise en œuvre l'exige).
	Enroll(ctx context.Context, req EnrollRequest) (*EnrollResult, error)
	// Detach : le VPS n'a plus accès au réseau (idempotent, même VPS injoignable).
	Detach(ctx context.Context, vpc VPC, vpsID uuid.UUID) error
	// Status : état des membres vu par le réseau.
	Status(ctx context.Context, vpc VPC) ([]MemberStatus, error)
	// Teardown : plus aucun VPS rattaché (idempotent).
	Teardown(ctx context.Context, vpc VPC) error
}

// Scripter : mise en œuvre dont le VPS s'inscrit lui-même par un script
// lancé en root (au premier démarrage ou à la main). apiURL : URL publique
// de l'API (…/kmanager/v1) ; token vide : lu en premier argument du script.
type Scripter interface {
	EnrollScript(apiURL, token string) string
}

// InstallEnrollmentTTL : jeton glissé dans le script de premier démarrage
// (l'installation par le fournisseur peut prendre du temps).
const InstallEnrollmentTTL = 2 * time.Hour

// ── Pare-feu (lot 4 ; interface posée au lot 1) ─────────────────────────

// Actions et protocoles d'une règle de pare-feu.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"

	ProtocolTCP  = "tcp"
	ProtocolUDP  = "udp"
	ProtocolICMP = "icmp"
	ProtocolAll  = "all"
)

// FirewallRule : règle d'entrée depuis Internet, dans l'ordre d'application.
type FirewallRule struct {
	Action   string `json:"action"`
	Protocol string `json:"protocol"`
	// PortFrom, PortTo : port ou plage (0 : tous) ; TCP et UDP seulement.
	PortFrom int `json:"port_from,omitempty"`
	PortTo   int `json:"port_to,omitempty"`
	// Source : adresse ou bloc (vide : toute adresse).
	Source string `json:"source,omitempty"`
	// Reserved : posée par la plateforme, jamais modifiable par le client.
	Reserved bool `json:"reserved,omitempty"`
}

// FirewallCapabilities : limites de la mise en œuvre.
type FirewallCapabilities struct {
	// MaxRules : règles du client, hors règles réservées.
	MaxRules int  `json:"max_rules"`
	Stateful bool `json:"stateful"`
	IPv6     bool `json:"ipv6"`
}

// FirewallProvider : filtrage depuis Internet des adresses publiques d'un
// VPS. Chaque mise en œuvre passe vpctest.RunFirewallContract.
type FirewallProvider interface {
	Name() string
	Capabilities() FirewallCapabilities
	// Apply : remplace toutes les règles du client (les règles réservées
	// sont ajoutées par la mise en œuvre).
	Apply(ctx context.Context, m Member, rules []FirewallRule) error
	// Get : règles appliquées, réservées comprises (Reserved vrai).
	Get(ctx context.Context, m Member) ([]FirewallRule, error)
}

// ── Erreurs ────────────────────────────────────────────────────────────

var (
	ErrUnavailable    = errors.New("private network not available for this VPS")
	ErrRegionMismatch = errors.New("VPS and project are not in the same region")
	ErrRangeExhausted = errors.New("no private range left for this project")
	ErrNotAttached    = errors.New("VPS is not attached to the private network")
	// ErrAlreadyAttached : interne (deux rattachements simultanés du même VPS).
	ErrAlreadyAttached   = errors.New("VPS already attached")
	ErrVPSNotReady       = errors.New("VPS is not ready for the private network")
	ErrInvalidEnrollment = errors.New("invalid or expired enrollment token")
	ErrNotEnrollable     = errors.New("this VPS does not need to enroll")
	ErrInvalidPublicKey  = errors.New("invalid public key")
	ErrTooManyRules      = errors.New("too many firewall rules")
	ErrInvalidRule       = errors.New("invalid firewall rule")
	ErrInvalidMonth      = errors.New("invalid month (expected YYYY-MM)")
)
