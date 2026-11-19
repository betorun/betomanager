package domain

import (
	"bytes"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Import d'un dump dans une base gérée. Seul le format personnalisé de pg_dump
// (-Fc) est accepté : pg_restore n'exécute que du SQL, avec l'utilisateur de la
// base (jamais un superutilisateur). Un dump en SQL brut passerait par psql,
// dont les méta-commandes (\!) lanceraient des commandes dans le Job.
const (
	ImportPending   = "PENDING"
	ImportRunning   = "RUNNING"
	ImportSucceeded = "SUCCEEDED"
	ImportFailed    = "FAILED"

	// DumpFormField : champ multipart du fichier.
	DumpFormField = "dump"
	// DefaultMaxDumpBytes : taille maximale d'un dump (2 Gio).
	DefaultMaxDumpBytes int64 = 2 << 30
)

// dumpMagic : en-tête d'un dump au format personnalisé de pg_dump.
var dumpMagic = []byte("PGDMP")

// IsCustomDump : le début du fichier est celui d'un dump pg_dump -Fc.
func IsCustomDump(head []byte) bool { return bytes.HasPrefix(head, dumpMagic) }

var (
	ErrNotADump        = errors.New("not a pg_dump custom-format file: create it with pg_dump -Fc (plain SQL dumps are not accepted)")
	ErrDumpTooLarge    = errors.New("dump too large: 2 GB at most")
	ErrImportRunning   = errors.New("an import is already running on this database")
	ErrImportNotFound  = errors.New("import not found")
	ErrImportsDisabled = errors.New("dump import is not available on this platform")
)

// DatabaseImport : un import.
type DatabaseImport struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	DatabaseID uuid.UUID `gorm:"type:uuid;not null" json:"database_id"`
	ProjectID  uuid.UUID `gorm:"type:uuid;not null" json:"-"`
	Status     string    `gorm:"not null" json:"status"`
	// Clean : objets existants supprimés avant la restauration (--clean).
	Clean     bool  `gorm:"not null" json:"clean"`
	SizeBytes int64 `gorm:"column:size_bytes;not null" json:"size_bytes"`
	// Failure : fin du journal de pg_restore quand l'import échoue.
	Failure    string     `gorm:"not null" json:"failure,omitempty"`
	CreatedBy  string     `gorm:"not null" json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func (DatabaseImport) TableName() string { return "database_imports" }

// Done : import terminé (réussi ou non).
func (i DatabaseImport) Done() bool { return i.Status == ImportSucceeded || i.Status == ImportFailed }

// ImportPath : un import d'une base.
type ImportPath struct {
	ProjectID  string `path:"id" binding:"required" json:"-"`
	DatabaseID string `path:"databaseID" binding:"required" json:"-"`
	ImportID   string `path:"importID" binding:"required" json:"-"`
}

// ImportUploadPath : envoi d'un dump (multipart : dump, clean).
type ImportUploadPath struct {
	ProjectID  string `path:"id" binding:"required" json:"-"`
	DatabaseID string `path:"databaseID" binding:"required" json:"-"`
}

// DumpPath : dump d'un import, lu par le Job (URL signée, sans session).
type DumpPath struct {
	ImportID string `path:"importID" binding:"required" json:"-"`
	Expires  int64  `query:"expires" json:"-"`
	Sig      string `query:"sig" json:"-"`
}
