package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sauvegardes des services qui en ont (Template.Backup non nil : MariaDB).
// Une copie chaque nuit, prise par un Job dans l'espace du projet et rangée
// dans le stockage d'objets ; les BackupsKept dernières réussies sont
// gardées. Une restauration prend d'abord une copie de l'état courant.

// États d'une sauvegarde.
const (
	BackupRunning   = "running"
	BackupCompleted = "completed"
	BackupFailed    = "failed"
)

// Déclencheurs.
const (
	TriggerNightly       = "nightly"
	TriggerManual        = "manual"
	TriggerBeforeRestore = "before_restore"
)

// BackupsKept : sauvegardes réussies gardées par service.
const BackupsKept = 7

// BackupCommands : scripts bash lancés dans l'image du service. Variables
// disponibles : ADDON_HOST (adresse du service), BACKUP_FILE (copie
// compressée), et le mot de passe du service sous le nom choisi par
// PasswordEnv (jamais en argument).
type BackupCommands struct {
	PasswordEnv string
	// Wait : attendre que le service réponde (réseau d'un pod neuf).
	Wait    string
	Dump    string
	Restore string
}

// Backup : une sauvegarde (table addon_backups).
type Backup struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	AddonID    uuid.UUID  `gorm:"type:uuid;not null" json:"-"`
	ProjectID  uuid.UUID  `gorm:"type:uuid;not null" json:"-"`
	Status     string     `gorm:"not null" json:"status"`
	Trigger    string     `gorm:"not null" json:"trigger"`
	ObjectKey  string     `gorm:"not null" json:"-"`
	SizeBytes  int64      `gorm:"not null" json:"size_bytes"`
	Error      string     `gorm:"not null" json:"error,omitempty"`
	CreatedBy  string     `gorm:"not null" json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	RestoredAt *time.Time `json:"restored_at,omitempty"`
}

func (Backup) TableName() string { return "addon_backups" }

// BackupList : sauvegardes d'un service (les plus récentes d'abord).
type BackupList struct {
	Backups []Backup `json:"backups"`
	Kept    int      `json:"kept"`
}

type BackupPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	AddonID   string `path:"addonID" binding:"required" description:"ID du service"`
	BackupID  string `path:"backupID" binding:"required" description:"ID de la sauvegarde"`
}

type RestoreRequest struct {
	ProjectID   string `path:"id" binding:"required" description:"ID du projet"`
	AddonID     string `path:"addonID" binding:"required" description:"ID du service"`
	BackupID    string `path:"backupID" binding:"required" description:"ID de la sauvegarde"`
	ConfirmName string `json:"confirm_name" binding:"required" description:"Nom du service, retapé pour confirmer"`
}

// BackupTransferPath : envoi ou lecture d'une copie par son Job (URL
// signée, sans session).
type BackupTransferPath struct {
	TaskID  string `path:"taskID" binding:"required" json:"-"`
	Expires int64  `query:"expires" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

var (
	ErrNoBackups           = errors.New("this service has no backups")
	ErrBackupsUnavailable  = errors.New("backups are not available on this platform")
	ErrBackupRunning       = errors.New("a backup or a restore of this service is already running")
	ErrBackupNotFound      = errors.New("backup not found")
	ErrBackupNotRestorable = errors.New("only a completed backup can be restored")
	ErrBackupTooLarge      = errors.New("the backup is too large")
	ErrConfirmMismatch     = errors.New("the confirmation does not match the service name")
)
