// Package service : cas d'usage des VPS et leurs ports (docs/modules/compute.md).
package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

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
	Start(ctx context.Context, providerID string) error
	Stop(ctx context.Context, providerID string) error
	Reboot(ctx context.Context, providerID string) error
	// Rebuild : réinstallation avec la clé SSH du client (aucun mot de passe envoyé).
	Rebuild(ctx context.Context, providerID, image, sshPublicKey string) error
	// ConsoleURL : accès console à courte durée.
	ConsoleURL(ctx context.Context, providerID string) (string, error)
	// Task : suivi d'une opération lancée chez le fournisseur.
	Task(ctx context.Context, providerID, taskID string) (*ProviderTask, error)
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
	// OrganizationNames : nom affiché de chaque organisation.
	OrganizationNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Starter : lancement d'un workflow (Temporal), identifiant imposé, jamais deux fois.
type Starter interface {
	Start(ctx context.Context, workflowID string, kind string, input any) error
}
