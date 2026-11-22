// Package domain : VPS revendus (docs/modules/compute.md). Le monolithe
// ignore quel fournisseur héberge un VPS : il retient le fournisseur et son
// identifiant chez lui, et passe par l'interface ComputeProvider.
package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// États d'un VPS, vus par le client.
const (
	StatePending   = "pending"   // commande enregistrée, pas encore lancée (outbox)
	StateCreating  = "creating"  // chez le fournisseur
	StateRunning   = "running"   // en marche
	StateStopped   = "stopped"   // arrêté
	StateSuspended = "suspended" // arrêté par la plateforme (impayé, abus)
	StateFailed    = "failed"    // création échouée (remboursée)
	StateDeleted   = "deleted"   // résilié
)

// Fournisseurs.
const ProviderOVH = "ovh"

// VPS : un serveur d'un client. ProviderID : son identifiant chez le
// fournisseur (serviceName pour OVH), vide tant qu'il n'est pas créé.
type VPS struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null" json:"organization_id"`
	ProjectID      uuid.UUID `gorm:"type:uuid;not null" json:"project_id"`
	Name           string    `gorm:"not null" json:"name"`
	Offer          string    `gorm:"not null" json:"offer"`
	Location       string    `gorm:"not null" json:"location"`
	Image          string    `gorm:"not null" json:"image"`
	Provider       string    `gorm:"not null" json:"-"`
	ProviderID     string    `gorm:"not null;default:''" json:"-"`
	State          string    `gorm:"not null" json:"state"`
	IPv4           string    `gorm:"column:ipv4;not null;default:''" json:"ipv4,omitempty"`
	IPv6           string    `gorm:"column:ipv6;not null;default:''" json:"ipv6,omitempty"`
	// SSHKeyFingerprint : empreinte de la clé installée (la clé elle-même
	// n'est gardée que le temps de la création).
	SSHKeyFingerprint string `gorm:"column:ssh_key_fingerprint;not null;default:''" json:"ssh_key_fingerprint,omitempty"`
	// Version : augmente à chaque changement d'état (les événements temps réel
	// plus anciens sont ignorés par la console).
	Version   int64      `gorm:"not null;default:1" json:"version"`
	PaidUntil *time.Time `json:"paid_until,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (VPS) TableName() string { return "compute_vps" }

// ── Offres ──────────────────────────────────────────────────────────────

// Emplacements et systèmes proposés (codes de la plateforme ; la
// correspondance avec le fournisseur est dans l'offre).
const (
	LocationGRA = "gra"
	LocationRBX = "rbx"
	LocationSBG = "sbg"
	ImageUbuntu = "ubuntu-24.04"
	ImageDebian = "debian-12"
)

// Offer : une offre vendue (VPS-1, VPS-2). Le prix de vente se calcule depuis
// ce que la plateforme paie au fournisseur : rien n'est fixé dans le code.
type Offer struct {
	Code string `gorm:"primaryKey" json:"code"`
	Name string `gorm:"not null" json:"name"`
	// ProviderPlanCode : référence de l'offre chez le fournisseur.
	ProviderPlanCode string `gorm:"not null;default:''" json:"provider_plan_code"`
	// ProviderPriceCents : prix mensuel payé au fournisseur, en centimes d'euro,
	// TTC si la TVA est facturée (0 : offre pas encore tarifée, invendable).
	ProviderPriceCents int64 `gorm:"not null;default:0" json:"provider_price_cents"`
	// CoefficientBP : coefficient de vente en points de base (20000 = ×2).
	CoefficientBP int64 `gorm:"column:coefficient_bp;not null" json:"coefficient_bp"`
	// RoundTo : arrondi au palier supérieur, en unités de la monnaie (500).
	RoundTo int64 `gorm:"not null" json:"round_to"`
	// Specs : caractéristiques affichées (vCPU, mémoire, disque, trafic).
	Specs     json.RawMessage `gorm:"type:jsonb;not null" json:"specs"`
	Locations json.RawMessage `gorm:"type:jsonb;not null" json:"locations"`
	// Images : système de la plateforme → image du fournisseur.
	Images    json.RawMessage `gorm:"type:jsonb;not null" json:"images"`
	Public    bool            `gorm:"not null" json:"public"`
	Position  int             `gorm:"not null" json:"position"`
	UpdatedBy string          `gorm:"not null;default:''" json:"updated_by"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (Offer) TableName() string { return "compute_offers" }

// MilliPerUnit : millièmes d'une unité de monnaie (comme la facturation).
const MilliPerUnit = 1000

// MonthlyPrice : prix mensuel de vente, en unités de la monnaie.
// eurRateMilli : unités de la monnaie pour 1 €, en millièmes (655957 pour le
// FCFA, parité fixe). 0 : invendable (offre non tarifée ou taux absent).
func (o Offer) MonthlyPrice(eurRateMilli int64) int64 {
	if o.ProviderPriceCents <= 0 || eurRateMilli <= 0 || o.CoefficientBP <= 0 {
		return 0
	}
	milli := o.ProviderPriceCents * eurRateMilli / 100 // millièmes d'unité
	milli = ceilDiv(milli*o.CoefficientBP, 10000)
	units := ceilDiv(milli, MilliPerUnit)
	if o.RoundTo > 1 {
		units = ceilDiv(units, o.RoundTo) * o.RoundTo
	}
	return units
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// LocationList / ImageMap : champs JSON lus (vides si illisibles).
func (o Offer) LocationList() []string {
	var out []string
	_ = json.Unmarshal(o.Locations, &out)
	return out
}

func (o Offer) ImageMap() map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(o.Images, &out)
	return out
}

// Sells : l'offre se vend à cet emplacement avec ce système.
func (o Offer) Sells(location, image string) bool {
	okLoc := false
	for _, l := range o.LocationList() {
		if l == location {
			okLoc = true
		}
	}
	_, okImg := o.ImageMap()[image]
	return okLoc && okImg
}

// ── Demandes (outbox) ───────────────────────────────────────────────────

// États d'une demande.
const (
	RequestPending = "pending" // écrite, pas encore lancée (Temporal absent : elle attend)
	RequestStarted = "started" // workflow lancé (WorkflowID = ID)
	RequestDone    = "done"
	RequestFailed  = "failed"
)

// Request : opération demandée sur un VPS, écrite dans la même transaction
// que la commande ou la facture ; le relais lance le workflow Temporal, avec
// WorkflowID = ID (jamais deux fois la même opération).
type Request struct {
	ID             uuid.UUID       `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID uuid.UUID       `gorm:"type:uuid;not null" json:"organization_id"`
	VPSID          *uuid.UUID      `gorm:"column:vps_id;type:uuid" json:"vps_id,omitempty"`
	Kind           string          `gorm:"not null" json:"kind"`
	Payload        json.RawMessage `gorm:"type:jsonb;not null" json:"-"`
	Status         string          `gorm:"not null" json:"status"`
	Attempts       int             `gorm:"not null;default:0" json:"-"`
	LastError      string          `gorm:"not null;default:''" json:"-"`
	CreatedBy      string          `gorm:"not null;default:''" json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
}

func (Request) TableName() string { return "compute_requests" }

// ── Secrets ─────────────────────────────────────────────────────────────

// Secret : mot de passe ou identifiants du mode secours, chiffrés, lus une
// seule fois puis effacés ; jamais dans l'historique Temporal (seul l'ID y passe).
type Secret struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey"`
	VPSID      uuid.UUID  `gorm:"column:vps_id;type:uuid;not null"`
	Kind       string     `gorm:"not null"`
	Ciphertext []byte     `gorm:"not null"`
	CreatedAt  time.Time  `gorm:"not null"`
	ExpiresAt  time.Time  `gorm:"not null"`
	ReadAt     *time.Time `gorm:""`
}

func (Secret) TableName() string { return "compute_secrets" }

// SecretTTL : un secret non lu disparaît après ce délai.
const SecretTTL = 15 * time.Minute

// ── Erreurs ─────────────────────────────────────────────────────────────

var (
	ErrOfferNotFound = errors.New("VPS offer not found")
	ErrInvalidOffer  = errors.New("invalid VPS offer: coefficient and rounding must be positive")
	ErrUnavailable   = errors.New("VPS are not available on this platform for now")
)

// NormalizeCode : « vps-1 » → « VPS-1 ».
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
