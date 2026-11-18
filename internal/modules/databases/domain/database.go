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
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
}

// DefaultVariable : variable posée sur le conteneur (convention des
// frameworks : Prisma, Django, Rails, Laravel…).
const DefaultVariable = "DATABASE_URL"

var variableRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ValidVariable : nom de variable d'environnement accepté.
func ValidVariable(n string) bool { return variableRe.MatchString(n) }

var (
	ErrNotReady         = errors.New("the database is not ready yet")
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
