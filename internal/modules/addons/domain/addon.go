// Package domain : services additionnels d'un projet (ADR 0013).
//
// Un service = une instance d'un modèle du catalogue (Template), portée par
// un conteneur ordinaire de sorte « service » : mêmes quotas, même facture,
// même cycle de vie. Le mot de passe est généré par l'API et posé comme
// variable SECRÈTE du conteneur porteur ; l'API ne le renvoie jamais, il
// n'est remis qu'aux conteneurs branchés, dans l'adresse de connexion.
package domain

import (
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// PasswordVariable : variable secrète du conteneur porteur qui garde le mot
// de passe ; la commande du modèle y fait référence par $(ADDON_PASSWORD).
const PasswordVariable = "ADDON_PASSWORD"

// DefaultMaxPerProject : services au plus par projet si le projet ne précise rien.
const DefaultMaxPerProject = 10

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,28}[a-z0-9]$`)

// ValidName : nom du service, qui est aussi celui du conteneur porteur et
// son adresse dans le projet (redis-cache:6379).
func ValidName(n string) bool { return nameRe.MatchString(n) }

var variableRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ValidVariable : nom de variable d'environnement accepté.
func ValidVariable(n string) bool { return variableRe.MatchString(n) }

var (
	ErrUnavailable      = errors.New("additional services are not available on this platform")
	ErrUnknownTemplate  = errors.New("unknown service: see the catalog")
	ErrInvalidName      = errors.New("invalid service name: 3 to 30 lowercase letters, digits and hyphens, starting with a letter")
	ErrInvalidVersion   = errors.New("this version is not offered for this service")
	ErrInvalidMode      = errors.New("this usage mode is not offered for this service")
	ErrInvalidSize      = errors.New("invalid size: see the sizes of the catalog")
	ErrInvalidDisk      = errors.New("invalid disk size: see the disks of the catalog")
	ErrNameTaken        = errors.New("a container or service with this name already exists in the project")
	ErrLimitReached     = errors.New("the project has reached its maximum number of services")
	ErrNotFound         = errors.New("service not found")
	ErrNotReady         = errors.New("the service is not ready yet")
	ErrWorkloadNotFound = errors.New("container not found in this project")
	ErrInvalidVariable  = errors.New("invalid variable name: uppercase letters, digits and underscores")
	ErrSelfAttach       = errors.New("a service cannot be attached to itself")
	ErrNoUpgrade        = errors.New("the service already runs this version or a newer one: versions never go back")
	ErrNotAnnounced     = errors.New("this service is not announced as coming soon")
	ErrNotAUser         = errors.New("only a user account can ask to be notified")
)

// Addon : une instance enregistrée (table project_addons).
type Addon struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID  uuid.UUID `gorm:"type:uuid;not null"`
	WorkloadID uuid.UUID `gorm:"type:uuid;not null"`
	Name       string    `gorm:"not null"`
	Template   string    `gorm:"not null"`
	Version    string    `gorm:"not null"`
	Mode       string    `gorm:"not null"`
	Size       string    `gorm:"not null"`
	Disk       string    `gorm:"not null"`
	CreatedBy  string    `gorm:"not null"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (Addon) TableName() string { return "project_addons" }

// Attachment : un branchement du service sur un conteneur.
type Attachment struct {
	AddonID      uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	WorkloadID   uuid.UUID `gorm:"type:uuid;primaryKey" json:"workload_id"`
	WorkloadName string    `gorm:"not null" json:"workload_name"`
	Variable     string    `gorm:"primaryKey" json:"variable"`
	CreatedAt    time.Time `json:"created_at"`
}

func (Attachment) TableName() string { return "addon_attachments" }

//  API

type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

type AddonPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	AddonID   string `path:"addonID" binding:"required" description:"ID du service"`
}

type CreateAddonRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	Template  string `json:"template" binding:"required" description:"Modèle du catalogue (ex. redis)"`
	Name      string `json:"name" binding:"required" description:"Nom du service, aussi son adresse dans le projet (3 à 30 caractères : minuscules, chiffres, tirets)"`
	Version   string `json:"version" description:"Version (défaut : la recommandée)"`
	Mode      string `json:"mode" description:"Usage (redis : persistent, par défaut, ou cache)"`
	Size      string `json:"size" description:"Taille : mini, small (défaut) ou medium"`
	Disk      string `json:"disk" description:"Disque en mode persistant : 1Gi (défaut), 2Gi ou 5Gi"`
}

type UpgradeRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	AddonID   string `path:"addonID" binding:"required" description:"ID du service"`
	Version   string `json:"version" description:"Version visée (défaut : la plus récente maintenue)"`
}

type AttachRequest struct {
	ProjectID  string `path:"id" binding:"required" description:"ID du projet"`
	AddonID    string `path:"addonID" binding:"required" description:"ID du service"`
	WorkloadID string `json:"workload_id" binding:"required" description:"ID du conteneur qui utilisera le service"`
	Variable   string `json:"variable" description:"Variable qui reçoit l'adresse de connexion (défaut selon le service, ex. REDIS_URL)"`
}

// AttachResponse : le service est branché ; le conteneur est redéployé avec
// la variable (secrète : jamais relisible).
type AttachResponse struct {
	WorkloadID string `json:"workload_id"`
	Variable   string `json:"variable"`
	Redeploy   bool   `json:"redeploy"`
}

// AddonView : un service, sans secret. Host n'est joignable que depuis les
// conteneurs du projet.
type AddonView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Template      string `json:"template"`
	Title         string `json:"title"`
	Logo          string `json:"logo"`
	Version       string `json:"version"`
	LatestVersion string `json:"latest_version"`
	// UpgradeAvailable : une version plus récente est maintenue ;
	// MajorUpgrade : elle change de version majeure (à confirmer).
	UpgradeAvailable bool   `json:"upgrade_available"`
	MajorUpgrade     bool   `json:"major_upgrade"`
	Mode             string `json:"mode,omitempty"`
	Size             string `json:"size"`
	Disk             string `json:"disk,omitempty"`
	// Status / Phase : ceux du conteneur porteur (RUNNING, STARTING…).
	Status      string       `json:"status"`
	Phase       string       `json:"phase,omitempty"`
	WorkloadID  string       `json:"workload_id"`
	Host        string       `json:"host"`
	Port        int          `json:"port"`
	Variable    string       `json:"variable"`
	Attachments []Attachment `json:"attachments"`
	CreatedAt   time.Time    `json:"created_at"`
}

// AddonList : services du projet et plafond.
type AddonList struct {
	Addons []AddonView `json:"addons"`
	Max    int         `json:"max"`
}

// DeleteResponse : conteneurs encore branchés (leur variable est gardée :
// la retirer sans prévenir les ferait planter au redémarrage).
type DeleteResponse struct {
	StillAttached []Attachment `json:"still_attached"`
}

// CatalogView : modèles ouverts, tailles proposées et services annoncés.
type CatalogView struct {
	Templates []Template `json:"templates"`
	Sizes     []Size     `json:"sizes"`
	Upcoming  []Upcoming `json:"upcoming"`
}

// WaitlistEntry : « Me prévenir » d'un utilisateur pour un service annoncé.
type WaitlistEntry struct {
	UserID     uuid.UUID `gorm:"type:uuid;primaryKey"`
	Template   string    `gorm:"primaryKey"`
	Locale     string    `gorm:"not null"`
	CreatedAt  time.Time
	NotifiedAt *time.Time
}

func (WaitlistEntry) TableName() string { return "addon_waitlist" }

// WaitlistPath : service annoncé visé.
type WaitlistPath struct {
	Template string `path:"template" binding:"required" description:"Service annoncé (ex. mariadb)"`
}

// WaitlistView : services annoncés pour lesquels l'utilisateur sera prévenu.
type WaitlistView struct {
	Templates []string `json:"templates"`
}

// Subscriber : à prévenir à l'ouverture d'un service.
type Subscriber struct {
	UserID   uuid.UUID
	Email    string
	FullName string
	Locale   string
}
