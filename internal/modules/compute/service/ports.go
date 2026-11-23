// Package service : cas d'usage des VPS et leurs ports (docs/modules/compute.md).
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// ComputeProvider : un fournisseur de VPS (OvhProvider, plus tard
// ProxmoxProvider). Appelé seulement depuis les activités Temporal du mode
// compute (seul process qui a les clés) ; jamais depuis une requête HTTP.
// Les deux implémentations passeront la même suite de tests de contrat.
type ComputeProvider interface {
	// Name : identifiant stable (domain.ProviderOVH…).
	Name() string
	// List : VPS du compte chez le fournisseur (réconciliation).
	List(ctx context.Context) ([]ProviderVM, error)
	// Get : un VPS ; domain-neutral (états traduits par l'adaptateur).
	Get(ctx context.Context, providerID string) (*ProviderVM, error)
	// Start, Stop, Reboot, Rebuild : rendent l'identifiant de l'opération
	// lancée chez le fournisseur (suivie par Task).
	Start(ctx context.Context, providerID string) (string, error)
	Stop(ctx context.Context, providerID string) (string, error)
	Reboot(ctx context.Context, providerID string) (string, error)
	// Rebuild : réinstallation avec la clé SSH du client (aucun mot de passe
	// envoyé) ; image : nom du système chez le fournisseur.
	// script : lancé au premier démarrage (vide : aucun).
	Rebuild(ctx context.Context, providerID, image, sshPublicKey, script string) (string, error)
	// ConsoleURL : accès console à courte durée.
	ConsoleURL(ctx context.Context, providerID string) (string, error)
	// Task : suivi d'une opération lancée chez le fournisseur.
	Task(ctx context.Context, providerID, taskID string) (*ProviderTask, error)
}

// BackupProvider : sauvegardes automatiques et option snapshot (mode compute).
type BackupProvider interface {
	// Backups : relevé (points de restauration, montés, snapshot, option).
	Backups(ctx context.Context, providerID string) (*ProviderBackups, error)
	Restore(ctx context.Context, providerID string, point time.Time, full bool) (string, error)
	Unmount(ctx context.Context, providerID string, point time.Time) (string, error)
	CreateSnapshot(ctx context.Context, providerID, description string) (string, error)
	RevertSnapshot(ctx context.Context, providerID string) (string, error)
	DeleteSnapshot(ctx context.Context, providerID string) (string, error)
	// AbortStuck : annule un snapshot ou une sauvegarde bloqué depuis plus de
	// maxAge ; vrai si une annulation a été demandée.
	AbortStuck(ctx context.Context, providerID string, maxAge time.Duration) (bool, error)
	// OrderSnapshotOption : commande l'option (une seule tentative), rend la commande.
	OrderSnapshotOption(ctx context.Context, providerID, planCode string, maxPriceCents int64) (string, error)
	// ReleaseSnapshotOption : option résiliée (planCode : celui de l'offre).
	ReleaseSnapshotOption(ctx context.Context, providerID, planCode string) error
}

// NetworkProvider : adresses d'un VPS (DNS inverse, blocage anti-spam) et
// fin du service chez le fournisseur.
type NetworkProvider interface {
	// SetReverse : DNS inverse de l'adresse (reverse vide : retiré).
	SetReverse(ctx context.Context, ip, reverse string) error
	// SpamBlocked : l'adresse IPv4 est bloquée pour envoi de spam (depuis quand).
	SpamBlocked(ctx context.Context, ipv4 string) (bool, time.Time, error)
	UnblockSpam(ctx context.Context, ipv4 string) error
	// StopRenewal : le service n'est plus renouvelé ; supprimé à son expiration.
	StopRenewal(ctx context.Context, providerID string) error
}

// StockProvider : disponibilité des offres chez le fournisseur.
type StockProvider interface {
	// Stock : état de chaque emplacement de la plateforme (gra, rbx, sbg) pour ce plan.
	Stock(ctx context.Context, planCode string) ([]ProviderStock, error)
}

// ProviderStock : un emplacement (code de la plateforme) et son stock.
type ProviderStock struct {
	Location string
	Status   string // domain.Stock*
	Days     int
}

// Inbox : e-mails récents de la boîte technique (incidents, abus…).
type Inbox interface {
	Recent(ctx context.Context, since time.Time) ([]InboxMail, error)
	Delete(ctx context.Context, id uint32) error
}

// InboxMail : un e-mail, texte seulement.
type InboxMail struct {
	ID      uint32
	From    string
	Subject string
	Date    time.Time
	Text    string
}

// RescueProvider : démarrage sur le système de secours (mode compute).
type RescueProvider interface {
	SetRescue(ctx context.Context, providerID string, on bool) error
}

// RescueMailbox : e-mails du fournisseur (identifiants du mode secours).
type RescueMailbox interface {
	FindRescue(ctx context.Context, providerID string, since time.Time) (*RescueMail, error)
	Delete(ctx context.Context, id uint32) error
	// Check : la boîte répond (diagnostic de l'équipe) ; nombre de messages.
	Check(ctx context.Context) (int, error)
}

// RescueMail : identifiants trouvés dans un e-mail (ID : pour l'effacer).
type RescueMail struct {
	ID          uint32
	Credentials domain.RescueCredentials
}

// Sealer : chiffrement des secrets en base (même clé que les autres secrets).
type Sealer interface {
	Seal(plain []byte, associated string) ([]byte, error)
	Open(sealed []byte, associated string) ([]byte, error)
}

// ProviderBackups : sauvegardes d'un VPS chez le fournisseur.
type ProviderBackups struct {
	Automated      bool
	RestorePoints  []time.Time // plus récent d'abord
	Mounted        []domain.MountedBackup
	Snapshot       *domain.SnapshotInfo
	SnapshotOption bool // option souscrite chez le fournisseur
}

// Orderer : commande de VPS chez le fournisseur (mode compute seulement).
type Orderer interface {
	// Order : panier, configuration, paiement par le moyen par défaut du
	// compte ; rend l'identifiant de la commande. Jamais rejoué : une erreur
	// après le paiement laisserait une seconde commande.
	Order(ctx context.Context, spec OrderSpec) (string, error)
	// OrderState : OrderPending, OrderDelivered (serviceName connu) ou OrderFailed.
	OrderState(ctx context.Context, orderID string) (state, providerID string, err error)
}

// OrderSpec : ce qui est commandé. MaxPriceCents : prix prévu (centimes
// d'euro, TTC) ; au-dessus, rien n'est payé (ErrProviderPriceAbove).
type OrderSpec struct {
	Reference     string // identifiant du VPS (description du panier)
	PlanCode      string
	Location      string // code de la plateforme (gra…)
	Image         string // nom du système chez le fournisseur
	MaxPriceCents int64
}

// États d'une commande chez le fournisseur.
const (
	OrderPending   = "pending"
	OrderDelivered = "delivered"
	OrderFailed    = "failed"
)

// ErrProviderPriceAbove : le fournisseur facture plus que le prix prévu.
var ErrProviderPriceAbove = errors.New("compute: provider price above the expected price")

// Wallet : solde du client (module billing).
type Wallet interface {
	ChargeComputeTx(ctx context.Context, tx *gorm.DB, org uuid.UUID, amount billingDomain.Milli, reference, description, by string) (bool, error)
	AfterComputeCharge(ctx context.Context, org uuid.UUID)
	RefundCompute(ctx context.Context, org uuid.UUID, amount billingDomain.Milli, reference, description string) error
}

// ProviderTask : une opération du fournisseur (installation, redémarrage…).
type ProviderTask struct {
	ID    string
	Type  string
	State string // TaskTodo, TaskDoing, TaskDone, TaskError, TaskCancelled
	// Progress : 0 à 100.
	Progress int
}

// États d'une opération du fournisseur.
const (
	TaskTodo      = "todo"
	TaskDoing     = "doing"
	TaskDone      = "done"
	TaskError     = "error"
	TaskCancelled = "cancelled"
)

// ProviderVM : un VPS vu par le fournisseur, traduit en termes de la plateforme.
type ProviderVM struct {
	ProviderID string
	State      string // domain.State*
	// RawState, Plan, Zone : termes du fournisseur (back-office seulement).
	RawState   string
	Plan       string
	Zone       string
	IPv4, IPv6 string
	// Busy : une opération du fournisseur est en cours (sauvegarde, installation).
	Busy bool
}

// Store : VPS, offres et demandes (repository).
type Store interface {
	Offers(ctx context.Context) ([]domain.Offer, error)
	Offer(ctx context.Context, code string) (*domain.Offer, error)
	SaveOffer(ctx context.Context, o *domain.Offer) error

	// EnqueueTx : écrit une demande dans la transaction de l'appelant (commande,
	// facture) : les deux réussissent ou échouent ensemble.
	EnqueueTx(tx *gorm.DB, r *domain.Request) error
	// ClaimPending : demandes en attente, verrouillées pour ce relais (SKIP LOCKED) ;
	// fn tourne dans la transaction.
	ClaimPending(ctx context.Context, limit int, fn func(tx *gorm.DB, rs []domain.Request) error) error
	MarkStarted(tx *gorm.DB, id uuid.UUID, at time.Time) error
	MarkAttemptFailed(tx *gorm.DB, id uuid.UUID, errMsg string) error
	FinishRequest(ctx context.Context, id uuid.UUID, ok bool, errMsg string, at time.Time) error
	Request(ctx context.Context, id uuid.UUID) (*domain.Request, error)

	// Réconciliation.
	// ProviderVPS : VPS de la plateforme créés chez ce fournisseur (identifiant connu).
	ProviderVPS(ctx context.Context, provider string) ([]domain.VPS, error)
	// ApplyObserved : état et adresses relevés chez le fournisseur ; rien
	// n'est écrit si le VPS a changé depuis la lecture (version). Rend vrai si écrit.
	ApplyObserved(ctx context.Context, id uuid.UUID, version int64, state, ipv4, ipv6 string, at time.Time) (bool, error)
	// ReplaceMachines : remplace le relevé du fournisseur.
	ReplaceMachines(ctx context.Context, provider string, ms []domain.ProviderMachine) error
	SaveRun(ctx context.Context, r *domain.ReconcileRun) error

	// Back-office.
	ListVPS(ctx context.Context, limit int) ([]domain.VPS, error)
	Machines(ctx context.Context) ([]domain.ProviderMachine, error)
	Runs(ctx context.Context, limit int) ([]domain.ReconcileRun, error)
	// VPS des clients.
	// Transaction : fn reçoit la transaction (VPS, débit, demandes ensemble).
	Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error
	CreateVPSTx(tx *gorm.DB, v *domain.VPS) error
	// UpdateVPSTx : champs donnés ; version augmentée.
	UpdateVPSTx(tx *gorm.DB, id uuid.UUID, fields map[string]any) error
	UpdateVPS(ctx context.Context, id uuid.UUID, fields map[string]any) error
	VPS(ctx context.Context, id uuid.UUID) (*domain.VPS, error)
	OrganizationVPS(ctx context.Context, org uuid.UUID) ([]domain.VPS, error)
	// DueVPS : VPS dont le mois payé est fini (renouvellement), au plus limit.
	DueVPS(ctx context.Context, now time.Time, limit int) ([]domain.VPS, error)
	ProjectOrganization(ctx context.Context, project uuid.UUID) (uuid.UUID, error)

	// Sauvegardes : relevé écrit par le mode compute, lu par l'API.
	SaveBackups(ctx context.Context, b *domain.Backups) error
	Backups(ctx context.Context, vps uuid.UUID) (*domain.Backups, error)

	// Secrets : SaveSecret remplace le secret de ce type du VPS ; TakeSecret
	// le rend une seule fois (marqué lu), s'il n'a pas expiré.
	SaveSecret(ctx context.Context, sec *domain.Secret) error
	TakeSecret(ctx context.Context, vps uuid.UUID, kind string, now time.Time) (*domain.Secret, error)
	DeleteSecrets(ctx context.Context, vps uuid.UUID, kind string) error

	// Stock, quotas, abus, e-mails du fournisseur (lot 8).
	ReplaceStock(ctx context.Context, offer string, rows []domain.Stock) error
	Stock(ctx context.Context) ([]domain.Stock, error)
	Limits(ctx context.Context) (*domain.Limits, error)
	SaveLimits(ctx context.Context, l *domain.Limits) error
	OrgLimits(ctx context.Context, org uuid.UUID) (*domain.OrgLimits, error)
	SaveOrgLimits(ctx context.Context, l *domain.OrgLimits) error
	DeleteOrgLimits(ctx context.Context, org uuid.UUID) error
	// CountVPS : VPS de l'organisation qui comptent dans le quota (ni résiliés ni en échec).
	CountVPS(ctx context.Context, org uuid.UUID) (int, error)
	// AccountAge / HasPaidTopup : ancienneté de l'organisation, recharge payée reçue.
	OrganizationCreatedAt(ctx context.Context, org uuid.UUID) (time.Time, error)
	HasPaidTopup(ctx context.Context, org uuid.UUID) (bool, error)
	SaveProviderMail(ctx context.Context, m *domain.ProviderMail) error
	ProviderMails(ctx context.Context, openOnly bool, limit int) ([]domain.ProviderMail, error)
	HandleProviderMail(ctx context.Context, id uuid.UUID, by string, at time.Time) error

	// OrganizationNames : nom affiché de chaque organisation.
	OrganizationNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Starter : lancement d'un workflow (Temporal), identifiant imposé, jamais deux fois.
type Starter interface {
	Start(ctx context.Context, workflowID string, kind string, input any) error
}
