package service

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Les sources des constats : chaque port est servi par le module qui
// possède la donnée (adaptateurs dans le routeur). Un port nil est une
// source absente (module désactivé) : ses constats sont omis.

// WorkloadState : un conteneur et son statut enregistré (tenu à jour par la
// surveillance du cluster).
type WorkloadState struct {
	ID        uuid.UUID
	Name      string
	Kind      string
	Status    string
	ManagedBy string
	UpdatedAt time.Time
}

// Workloads : conteneurs du projet.
type Workloads interface {
	List(ctx context.Context, project uuid.UUID) ([]WorkloadState, error)
	// Restarts : redémarrages en cours (somme des instances) ; lu en direct.
	Restarts(ctx context.Context, workload uuid.UUID) (int, error)
}

// BuildState : un build récent.
type BuildState struct {
	ID         uuid.UUID
	Name       string
	Status     string
	Failure    string
	WorkloadID *uuid.UUID
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// Builds : builds récents du projet (du plus récent au plus ancien).
type Builds interface {
	List(ctx context.Context, project uuid.UUID) ([]BuildState, error)
}

// BudgetState : budget mensuel du projet (Limit 0 : pas de budget).
type BudgetState struct {
	Limit, Spent int64
	Stopped      bool
}

// Budgets : budget du projet.
type Budgets interface {
	Budget(ctx context.Context, project uuid.UUID) (*BudgetState, error)
}

// DiskState : dernière mesure d'un disque.
type DiskState struct {
	// Kind : workload ou database.
	Kind       string
	TargetID   uuid.UUID
	TargetName string
	Used       int64
	Capacity   int64
	MeasuredAt time.Time
}

// Disks : disques mesurés du projet.
type Disks interface {
	List(ctx context.Context, project uuid.UUID) ([]DiskState, error)
}

// DomainState : un domaine personnalisé.
type DomainState struct {
	ID        uuid.UUID
	Hostname  string
	Verified  bool
	CreatedAt time.Time
}

// WorkloadDomains : domaines d'un conteneur.
type WorkloadDomains interface {
	List(ctx context.Context, workload uuid.UUID) ([]DomainState, error)
}

// SiteState : un site statique.
type SiteState struct {
	ID   uuid.UUID
	Name string
}

// Sites : sites du projet et leurs domaines.
type Sites interface {
	List(ctx context.Context, project uuid.UUID) ([]SiteState, error)
	Domains(ctx context.Context, site uuid.UUID) ([]DomainState, error)
}

// DatabaseState : une base gérée.
type DatabaseState struct {
	ID        uuid.UUID
	Name      string
	Status    string
	UpdatedAt time.Time
	// Attached : conteneurs branchés.
	Attached int
}

// Databases : bases du projet.
type Databases interface {
	List(ctx context.Context, project uuid.UUID) ([]DatabaseState, error)
}

// AddonState : un service additionnel (statut de son conteneur porteur).
type AddonState struct {
	ID     string
	Name   string
	Status string
}

// Addons : services du projet.
type Addons interface {
	List(ctx context.Context, project uuid.UUID) ([]AddonState, error)
}

// Projects : nom d'un projet (constats de budget).
type Projects interface {
	Name(ctx context.Context, project uuid.UUID) (string, error)
}

// Cache : résultats gardés peu de temps (Redis).
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Logger : journal des sources illisibles.
type Logger interface {
	Warnw(msg string, keysAndValues ...any)
}
