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
	// MonthlyPrice / Currency : prix du dernier mois débité (unités de la monnaie).
	MonthlyPrice int64  `gorm:"not null;default:0" json:"monthly_price"`
	Currency     string `gorm:"not null;default:''" json:"currency"`
	// ProviderOrderID : commande chez le fournisseur (jamais passée deux fois).
	ProviderOrderID string `gorm:"not null;default:''" json:"-"`
	// FailureCode : raison d'un échec ou d'une suspension (domain.Failure*).
	FailureCode string    `gorm:"not null;default:''" json:"failure_code,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (VPS) TableName() string { return "compute_vps" }

//  Offres

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

// Sellable : l'offre est publique, tarifée et rattachée à un plan du fournisseur.
func (o Offer) Sellable(eurRateMilli int64) bool {
	return o.Public && o.ProviderPlanCode != "" && o.MonthlyPrice(eurRateMilli) > 0
}

// Sells : l'offre se vend à cet emplacement avec ce système.
func (o Offer) Sells(location, image string) bool {
	okLoc := false
	for _, l := range o.LocationList() {
		if l == location {
			okLoc = true
		}
	}
	// Système sans image du fournisseur : pas encore réglé, invendable.
	return okLoc && o.ImageMap()[image] != ""
}

//	Demandes (outbox)
//
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

//	Secrets
//
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

// Raisons d'un échec ou d'une suspension (FailureCode).
const (
	FailureProvider     = "provider"      // commande ou installation refusée par le fournisseur
	FailurePriceChanged = "price_changed" // le fournisseur facture plus que prévu : rien commandé
	FailureTimeout      = "timeout"       // livraison trop longue
	FailureUnpaid       = "unpaid"        // renouvellement non payé (suspendu)
)

//  Erreurs

var (
	ErrOfferNotFound            = errors.New("VPS offer not found")
	ErrInvalidOffer             = errors.New("invalid VPS offer: coefficient and rounding must be positive")
	ErrUnavailable              = errors.New("VPS are not available on this platform for now")
	ErrNotSold                  = errors.New("this VPS offer is not sold with this location and system")
	ErrPriceChanged             = errors.New("the VPS price has changed: check the new price")
	ErrInvalidName              = errors.New("invalid VPS name: 1 to 63 lowercase letters, digits or hyphens")
	ErrNameTaken                = errors.New("a VPS with this name already exists in this project")
	ErrInvalidSSHKey            = errors.New("invalid SSH public key: ed25519, ecdsa or RSA (2048 bits or more)")
	ErrVPSNotFound              = errors.New("VPS not found")
	ErrProjectNotInOrganization = errors.New("project not found in this organization")
)

// ValidName : étiquette DNS (le nom devient le nom d'hôte).
func ValidName(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Period : référence du mois qui commence à from (débit idempotent).
func Period(vps uuid.UUID, from time.Time) string {
	return "compute:vps:" + vps.String() + ":" + from.UTC().Format("2006-01-02")
}

// NextPeriod : fin du mois payé qui commence à from, bornée au dernier jour
// du mois suivant (31 janvier → 28 février, pas 3 mars).
func NextPeriod(from time.Time) time.Time {
	y, m, d := from.Date()
	last := time.Date(y, m+2, 0, 0, 0, 0, 0, from.Location()).Day() // dernier jour du mois suivant
	if d > last {
		d = last
	}
	h, mi, sec := from.Clock()
	return time.Date(y, m+1, d, h, mi, sec, from.Nanosecond(), from.Location())
}

// NormalizeCode : « vps-1 » → « VPS-1 ».
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

//  Réconciliation (base ↔ fournisseur)

// Écarts relevés par la réconciliation (vue de l'équipe seulement).
const (
	// DriftOrphan : VPS chez le fournisseur, inconnu de la plateforme (payé, vendu à personne).
	DriftOrphan = "orphan"
	// DriftState : état différent, que la réconciliation ne corrige pas
	// (VPS suspendu encore en marche, en création, résilié…).
	DriftState = "state"
	// DriftMissing : VPS de la plateforme absent chez le fournisseur.
	DriftMissing = "missing"
)

// ProviderMachine : dernier relevé d'un VPS chez le fournisseur (table
// compute_provider_vms), remplacé à chaque réconciliation.
type ProviderMachine struct {
	Provider   string     `gorm:"primaryKey" json:"provider"`
	ProviderID string     `gorm:"primaryKey" json:"provider_id"`
	VPSID      *uuid.UUID `gorm:"column:vps_id;type:uuid" json:"vps_id,omitempty"`
	// State : état traduit (domain.State*) ; RawState : celui du fournisseur.
	State    string    `gorm:"not null" json:"state"`
	RawState string    `gorm:"not null;default:''" json:"raw_state"`
	Plan     string    `gorm:"not null;default:''" json:"plan"`
	Zone     string    `gorm:"not null;default:''" json:"zone"`
	IPv4     string    `gorm:"column:ipv4;not null;default:''" json:"ipv4"`
	IPv6     string    `gorm:"column:ipv6;not null;default:''" json:"ipv6"`
	Busy     bool      `gorm:"not null;default:false" json:"busy"`
	Drift    string    `gorm:"not null;default:''" json:"drift"`
	SeenAt   time.Time `gorm:"not null" json:"seen_at"`
}

func (ProviderMachine) TableName() string { return "compute_provider_vms" }

// ReconcileRun : une passe de réconciliation (table compute_reconcile_runs).
type ReconcileRun struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Provider   string     `gorm:"not null" json:"provider"`
	StartedAt  time.Time  `gorm:"not null" json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Seen       int        `gorm:"not null;default:0" json:"seen"`
	Updated    int        `gorm:"not null;default:0" json:"updated"`
	Orphans    int        `gorm:"not null;default:0" json:"orphans"`
	Missing    int        `gorm:"not null;default:0" json:"missing"`
	Drifts     int        `gorm:"not null;default:0" json:"drifts"`
	Error      string     `gorm:"not null;default:''" json:"error,omitempty"`
}

func (ReconcileRun) TableName() string { return "compute_reconcile_runs" }

//  Types de demande (outbox)

// Opérations sur un VPS, exécutées par le mode compute.
const (
	KindCreate = "vm.create"
	KindStart  = "vm.start"
	KindStop   = "vm.stop"
)

// CreatePayload : demande de création. La clé publique n'est pas un secret
// (elle passe dans l'historique Temporal) ; Period et AmountMilli servent au
// remboursement en cas d'échec.
type CreatePayload struct {
	SSHPublicKey string `json:"ssh_public_key"`
	Period       string `json:"period"`
	AmountMilli  int64  `json:"amount_milli"`
}
