// Package domain : bases de données gérées d'un projet.
//
// Une base = un Cluster CloudNativePG (postgresql.cnpg.io/v1) dans l'espace
// de noms du projet, une instance, un disque. CloudNativePG crée l'utilisateur
// et la base « app », et le secret <cluster>-app qui porte l'URI de
// connexion. L'API ne renvoie jamais le mot de passe : il n'est remis qu'aux
// conteneurs du projet (voir « attacher », lot suivant).
package domain

import (
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	EnginePostgres = "postgres"

	StatusProvisioning = "PROVISIONING"
	StatusReady        = "READY"
	StatusFailed       = "FAILED"
	StatusDeleting     = "DELETING"
	// StatusStopped : base arrêtée (hibernation CloudNativePG : plus de pod,
	// le disque et les données sont gardés).
	StatusStopped = "STOPPED"

	// Port, base et utilisateur créés par CloudNativePG (bootstrap initdb).
	Port         = 5432
	DatabaseName = "app"
	UserName     = "app"

	DefaultStorage = "1Gi"
)

// AllowedStorage : tailles proposées (le disque compte dans le quota du projet).
var AllowedStorage = map[string]bool{"1Gi": true, "2Gi": true, "5Gi": true, "10Gi": true, "20Gi": true}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,28}[a-z0-9]$`)

var (
	ErrInvalidName    = errors.New("invalid database name: 3 to 30 lowercase letters, digits and hyphens, starting with a letter")
	ErrInvalidStorage = errors.New("invalid storage size: 1Gi, 2Gi, 5Gi, 10Gi or 20Gi")
	ErrNameTaken      = errors.New("a database with this name already exists in the project")
	ErrNotFound       = errors.New("database not found")
	ErrUnavailable    = errors.New("managed databases are not available on this platform")
)

// ValidName : nom de base valide (il entre dans les noms Kubernetes).
func ValidName(n string) bool { return nameRe.MatchString(n) }

// Database : une base gérée.
type Database struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID   uuid.UUID `gorm:"type:uuid;not null"`
	Name        string    `gorm:"not null"`
	Engine      string    `gorm:"not null"`
	StorageSize string    `gorm:"not null"`
	Status      string    `gorm:"not null"`
	CreatedBy   string    `gorm:"not null"`
	// RestoredFrom / RestoreTarget : base née des sauvegardes d'une autre,
	// à cet instant (nil : le plus récent possible).
	RestoredFrom  *uuid.UUID `gorm:"type:uuid"`
	RestoreTarget *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (Database) TableName() string { return "databases" }

// ClusterName : nom du Cluster CloudNativePG (et préfixe de ses objets).
func (d Database) ClusterName() string { return "db-" + d.Name }

//  API

type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

type DatabasePath struct {
	ProjectID  string `path:"id" binding:"required" description:"ID du projet"`
	DatabaseID string `path:"databaseID" binding:"required" description:"ID de la base de données"`
}

type CreateDatabaseRequest struct {
	ProjectID   string `path:"id" binding:"required" description:"ID du projet"`
	Name        string `json:"name" binding:"required" description:"Nom de la base (3 à 30 caractères : minuscules, chiffres, tirets)"`
	StorageSize string `json:"storage_size" description:"Taille du disque : 1Gi (défaut), 2Gi, 5Gi, 10Gi ou 20Gi"`
}

// DatabaseView : une base, sans secret. Host n'est joignable que depuis les
// conteneurs du projet.
type DatabaseView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Engine      string    `json:"engine"`
	Version     string    `json:"version,omitempty"`
	StorageSize string    `json:"storage_size"`
	Status      string    `json:"status"`
	Phase       string    `json:"phase,omitempty"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Database    string    `json:"database"`
	User        string    `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	Backups     Backups   `json:"backups"`
	// RestoredFrom : base source si celle-ci est une restauration.
	RestoredFrom  string     `json:"restored_from,omitempty"`
	RestoreTarget *time.Time `json:"restore_target,omitempty"`
	// Attachments : conteneurs branchés sur la base (et leur variable).
	Attachments []Attachment `json:"attachments"`
	// LastImport : dernier import de dump, s'il y en a eu un.
	LastImport *DatabaseImport `json:"last_import,omitempty"`
	// ResetSnapshot : copie prise avant la dernière remise à zéro, encore
	// restaurable (absente sinon).
	ResetSnapshot *Snapshot `json:"reset_snapshot,omitempty"`
}

// Attachment : un branchement de la base sur un conteneur.
type Attachment struct {
	DatabaseID   uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	WorkloadID   uuid.UUID `gorm:"type:uuid;primaryKey" json:"workload_id"`
	WorkloadName string    `gorm:"not null" json:"workload_name"`
	Variable     string    `gorm:"primaryKey" json:"variable"`
	CreatedAt    time.Time `json:"created_at"`
}

func (Attachment) TableName() string { return "database_attachments" }

// Backups : sauvegardes continues (journal) et complètes (chaque nuit).
type Backups struct {
	Enabled bool `json:"enabled"`
	// LastAt / LastStatus : dernière sauvegarde complète (completed, running, failed…).
	LastAt     *time.Time `json:"last_at,omitempty"`
	LastStatus string     `json:"last_status,omitempty"`
}

// DefaultVariable : variable posée sur le conteneur (convention des
// frameworks : Prisma, Django, Rails, Laravel…).
const DefaultVariable = "DATABASE_URL"

var variableRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ValidVariable : nom de variable d'environnement accepté.
func ValidVariable(n string) bool { return variableRe.MatchString(n) }

var (
	ErrNotReady         = errors.New("the database is not ready yet")
	ErrStopped          = errors.New("the database is stopped: start it first")
	ErrNotStopped       = errors.New("the database is not stopped")
	ErrInvalidVariable  = errors.New("invalid variable name: uppercase letters, digits and underscores")
	ErrWorkloadNotFound = errors.New("container not found in this project")
)

type AttachRequest struct {
	ProjectID  string `path:"id" binding:"required" description:"ID du projet"`
	DatabaseID string `path:"databaseID" binding:"required" description:"ID de la base de données"`
	WorkloadID string `json:"workload_id" binding:"required" description:"ID du conteneur qui utilisera la base"`
	Variable   string `json:"variable" description:"Variable qui reçoit l'URI de connexion (défaut : DATABASE_URL)"`
}

// AttachResponse : la base est branchée ; le conteneur est redéployé avec la
// variable (secrète : jamais relisible).
type AttachResponse struct {
	WorkloadID string `json:"workload_id"`
	Variable   string `json:"variable"`
	Redeploy   bool   `json:"redeploy"`
}

var (
	ErrNoBackup      = errors.New("this database has no completed backup to restore from")
	ErrInvalidTarget = errors.New("the restore time must be in the past")
)

// RestoreRequest : nouvelle base, née des sauvegardes de la base :databaseID.
type RestoreRequest struct {
	ProjectID  string     `path:"id" binding:"required" description:"ID du projet"`
	DatabaseID string     `path:"databaseID" binding:"required" description:"ID de la base de données"`
	Name       string     `json:"name" binding:"required" description:"Nom de la nouvelle base (la base source n'est jamais modifiée)"`
	TargetTime *time.Time `json:"target_time" description:"Instant à restaurer, RFC 3339 (défaut : le plus récent)"`
}
