package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Contenu d'une base, inspection d'un dump avant import, et remise à zéro
// avec copie de sécurité.

// Contents : ce que contient une base (mesuré à la demande).
type Contents struct {
	SizeBytes  int64          `json:"size_bytes"`
	Tables     int            `json:"tables"`
	Schemas    []SchemaTables `json:"schemas"`
	MeasuredAt time.Time      `json:"measured_at"`
}

// InspectResult : un dump comparé à la base visée.
type InspectResult struct {
	Dump     DumpSummary `json:"dump"`
	Database Contents    `json:"database"`
	// Conflicts : schémas qui ont des tables dans le dump ET dans la base.
	Conflicts []string `json:"conflicts"`
	// Foreign : la base a des tables, mais aucune dans les schémas du dump
	// (sans doute la mauvaise base).
	Foreign bool `json:"foreign"`
}

// Compare : conflits et base étrangère au dump.
func Compare(dump DumpSummary, db Contents) InspectResult {
	out := InspectResult{Dump: dump, Database: db, Conflicts: []string{}}
	inDump := map[string]int{}
	for _, s := range dump.Schemas {
		inDump[s.Name] = s.Tables
	}
	shared := false
	for _, s := range db.Schemas {
		if s.Tables == 0 {
			continue
		}
		if n, ok := inDump[s.Name]; ok {
			shared = true
			if n > 0 {
				out.Conflicts = append(out.Conflicts, s.Name)
			}
		}
	}
	out.Foreign = db.Tables > 0 && dump.Tables > 0 && !shared
	return out
}

// InspectHeadBytes : début du fichier lu par l'inspection (la table des
// matières est en tête ; la console n'envoie que ces octets).
const InspectHeadBytes = 16 << 20

// SnapshotRetention : durée de conservation de la copie prise avant une
// remise à zéro.
const SnapshotRetention = 24 * time.Hour

// Snapshot : copie de sécurité d'une base, prise avant sa remise à zéro.
type Snapshot struct {
	DatabaseID uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	// ObjectKey : le pg_dump dans le stockage d'objets.
	ObjectKey string    `gorm:"not null" json:"-"`
	SizeBytes int64     `gorm:"not null" json:"size_bytes"`
	TakenBy   string    `gorm:"not null" json:"-"`
	TakenAt   time.Time `gorm:"not null" json:"taken_at"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	// RestoreImportID : dernier import lancé pour restaurer cette copie.
	RestoreImportID *uuid.UUID `gorm:"type:uuid" json:"-"`
	// RestoredAt : fin de la restauration réussie (calculé, pas en base).
	RestoredAt *time.Time `gorm:"-" json:"restored_at,omitempty"`
}

func (Snapshot) TableName() string { return "database_snapshots" }

// Expired : copie trop ancienne pour être restaurée.
func (s Snapshot) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// ResetRequest : remise à zéro ; le nom de la base, retapé, confirme.
type ResetRequest struct {
	ProjectID   string `path:"id" binding:"required" description:"ID du projet"`
	DatabaseID  string `path:"databaseID" binding:"required" description:"ID de la base de données"`
	ConfirmName string `json:"confirm_name" binding:"required" description:"Nom de la base, retapé pour confirmer"`
}

// ResetResponse : la base est vide ; la copie se restaure jusqu'à ExpiresAt.
type ResetResponse struct {
	Snapshot Snapshot `json:"snapshot"`
}

// SnapshotUploadPath : envoi de la copie par le Job (URL signée, sans session).
type SnapshotUploadPath struct {
	TaskID  string `path:"taskID" binding:"required" json:"-"`
	Expires int64  `query:"expires" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

var (
	ErrConfirmMismatch = errors.New("the confirmation does not match the database name")
	ErrNoSnapshot      = errors.New("no safety copy to restore: it is kept 24 hours after the database was emptied")
	ErrShellDisabled   = errors.New("reading and emptying databases is not available on this platform")
)
