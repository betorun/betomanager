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
	FailureCode string `gorm:"not null;default:''" json:"failure_code"`
	// SnapshotOption : option snapshot (domain.Snapshot*), vide si non souscrite.
	SnapshotOption string `gorm:"not null;default:''" json:"snapshot_option"`
	// Operation : action en cours demandée par le client (domain.Op*), vide sinon.
	Operation string `gorm:"not null;default:''" json:"operation"`
	// ResumeState : état à retrouver quand la suspension de l'organisation
	// est levée (running ou stopped).
	ResumeState string    `gorm:"not null;default:''" json:"-"`
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
	Images json.RawMessage `gorm:"type:jsonb;not null" json:"images"`
	// SnapshotPlanCode / SnapshotPriceCents : option snapshot chez le
	// fournisseur (même coefficient et même arrondi que l'offre ; 0 : non vendue).
	SnapshotPlanCode   string    `gorm:"not null;default:''" json:"snapshot_plan_code"`
	SnapshotPriceCents int64     `gorm:"not null;default:0" json:"snapshot_price_cents"`
	Public             bool      `gorm:"not null" json:"public"`
	Position           int       `gorm:"not null" json:"position"`
	UpdatedBy          string    `gorm:"not null;default:''" json:"updated_by"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (Offer) TableName() string { return "compute_offers" }

// MilliPerUnit : millièmes d'une unité de monnaie (comme la facturation).
const MilliPerUnit = 1000

// Rate : grille de prix d'une monnaie (module billing) : unités de la
// monnaie pour 1 €, en millièmes (655957 pour le FCFA, parité fixe), et
// palier d'arrondi de la grille, en unités de cette monnaie (500 FCFA).
type Rate struct {
	Currency string
	EURMilli int64
	RoundTo  int64
}

// Price : prix mensuel de vente dans la monnaie de la grille, en unités.
// Arrondi au palier de la grille (celui de l'offre si la grille n'en a pas) :
// un palier exprimé en FCFA ne s'applique jamais à une autre monnaie.
// 0 : invendable (offre non tarifée ou taux absent).
func (o Offer) Price(r Rate) int64 {
	return price(o.ProviderPriceCents, o.CoefficientBP, roundFor(r, o.RoundTo), r.EURMilli)
}

// SnapshotPrice : prix mensuel de l'option snapshot (0 : non vendue).
func (o Offer) SnapshotPrice(r Rate) int64 {
	if o.SnapshotPlanCode == "" {
		return 0
	}
	return price(o.SnapshotPriceCents, o.CoefficientBP, roundFor(r, o.RoundTo), r.EURMilli)
}

func roundFor(r Rate, fallback int64) int64 {
	if r.RoundTo > 0 {
		return r.RoundTo
	}
	return fallback
}

// MonthlyPrice : Price avec le palier de l'offre (grille sans palier).
func (o Offer) MonthlyPrice(eurRateMilli int64) int64 { return o.Price(Rate{EURMilli: eurRateMilli}) }

func price(cents, coefficientBP, roundTo, eurRateMilli int64) int64 {
	if cents <= 0 || eurRateMilli <= 0 || coefficientBP <= 0 {
		return 0
	}
	milli := cents * eurRateMilli / 100 // millièmes d'unité
	milli = ceilDiv(milli*coefficientBP, 10000)
	units := ceilDiv(milli, MilliPerUnit)
	if roundTo > 1 {
		units = ceilDiv(units, roundTo) * roundTo
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
func (o Offer) Sellable(r Rate) bool {
	return o.Public && o.ProviderPlanCode != "" && o.Price(r) > 0
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
	LastError      string          `gorm:"not null;default:''" json:"last_error,omitempty"`
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
	FailureOrgSuspended = "org_suspended" // organisation suspendue (solde épuisé)
)

// Opérations en cours (Operation), une à la fois par VPS.
const (
	OpStarting     = "starting"
	OpStopping     = "stopping"
	OpRebooting    = "rebooting"
	OpReinstalling = "reinstalling"
	// Sauvegardes (lot 5).
	OpRestoring        = "restoring"
	OpMounting         = "mounting"
	OpUnmounting       = "unmounting"
	OpSnapshotting     = "snapshotting"
	OpReverting        = "reverting"
	OpDeletingSnapshot = "deleting_snapshot"
	OpSubscribing      = "subscribing"
	OpUnsubscribing    = "unsubscribing"
)

// État de l'option snapshot (SnapshotOption).
const (
	SnapshotOrdering  = "ordering"
	SnapshotActive    = "active"
	SnapshotReleasing = "releasing"
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
	ErrBusy                     = errors.New("an operation is already running on this VPS: wait for it to finish")
	ErrNotReady                 = errors.New("this VPS is not ready for this action")
	ErrSuspended                = errors.New("this VPS is suspended: top up the balance to restart it")
	ErrBackupsUnavailable       = errors.New("automated backups are not active on this VPS")
	ErrSnapshotNotSubscribed    = errors.New("the snapshot option is not active on this VPS")
	ErrSnapshotExists           = errors.New("this VPS already has a snapshot: delete it first")
	ErrNoSnapshot               = errors.New("this VPS has no snapshot")
	ErrRestorePointNotFound     = errors.New("restore point not found")
	ErrSnapshotNotSold          = errors.New("the snapshot option is not sold with this offer")
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
	KindCreate    = "vm.create"
	KindStart     = "vm.start"
	KindStop      = "vm.stop"
	KindReboot    = "vm.reboot"
	KindReinstall = "vm.reinstall"
	// Sauvegardes (lot 5).
	KindRestore         = "vm.backup.restore"
	KindUnmount         = "vm.backup.unmount"
	KindSnapshotCreate  = "vm.snapshot.create"
	KindSnapshotRevert  = "vm.snapshot.revert"
	KindSnapshotDelete  = "vm.snapshot.delete"
	KindSnapshotOrder   = "vm.snapshot.subscribe"
	KindSnapshotRelease = "vm.snapshot.unsubscribe"
)

// RestorePayload : point de restauration et mode (full : remplace le disque ;
// mount : attaché en disque supplémentaire).
type RestorePayload struct {
	RestorePoint time.Time `json:"restore_point"`
	Mode         string    `json:"mode"`
}

// Modes de restauration.
const (
	RestoreFull  = "full"
	RestoreMount = "mount"
)

// SnapshotPayload : description du snapshot à créer.
type SnapshotPayload struct {
	Description string `json:"description"`
}

// SubscribePayload : mois de l'option débité (remboursé si l'option n'est pas obtenue).
type SubscribePayload struct {
	Period      string `json:"period"`
	AmountMilli int64  `json:"amount_milli"`
}

// ── Sauvegardes (relevé du fournisseur) ─────────────────────────────────

// Backups : sauvegardes d'un VPS relevées par le mode compute (table
// compute_vps_backups) ; l'API lit ce relevé, jamais le fournisseur.
type Backups struct {
	VPSID         uuid.UUID       `gorm:"column:vps_id;type:uuid;primaryKey" json:"-"`
	Automated     bool            `gorm:"not null;default:false" json:"automated"`
	RestorePoints json.RawMessage `gorm:"type:jsonb;not null" json:"-"`
	Mounted       json.RawMessage `gorm:"type:jsonb;not null" json:"-"`
	Snapshot      json.RawMessage `gorm:"type:jsonb" json:"-"`
	RefreshedAt   time.Time       `gorm:"not null" json:"refreshed_at"`
}

func (Backups) TableName() string { return "compute_vps_backups" }

// MountedBackup : point de restauration attaché au VPS.
type MountedBackup struct {
	RestorePoint time.Time `json:"restore_point"`
	Disk         string    `json:"disk"`
}

// SnapshotInfo : le snapshot du VPS (un seul).
type SnapshotInfo struct {
	CreatedAt   time.Time `json:"created_at"`
	Description string    `json:"description"`
}

// ReinstallPayload : système et clé de la réinstallation demandée par le client.
type ReinstallPayload struct {
	Image        string `json:"image"`
	SSHPublicKey string `json:"ssh_public_key"`
	Fingerprint  string `json:"fingerprint"`
}

// CreatePayload : demande de création. La clé publique n'est pas un secret
// (elle passe dans l'historique Temporal) ; Period et AmountMilli servent au
// remboursement en cas d'échec.
type CreatePayload struct {
	SSHPublicKey string `json:"ssh_public_key"`
	Period       string `json:"period"`
	AmountMilli  int64  `json:"amount_milli"`
}
