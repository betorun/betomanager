package domain

import (
	"time"

	"github.com/google/uuid"
)

// Archive : copie finale d'une base, prise juste avant sa suppression par la
// plateforme (organisation suspendue trop longtemps, ADR 0012). La base
// n'existe plus : la copie garde son nom et son projet, et reste dans le
// stockage d'objets jusqu'à ExpiresAt (restauration par l'équipe).
type Archive struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null"`
	ProjectID      uuid.UUID `gorm:"type:uuid;not null"`
	DatabaseID     uuid.UUID `gorm:"type:uuid;not null"`
	DatabaseName   string    `gorm:"not null"`
	ObjectKey      string    `gorm:"not null"`
	SizeBytes      int64     `gorm:"not null"`
	Reason         string    `gorm:"not null"`
	CreatedAt      time.Time `gorm:"not null"`
	ExpiresAt      time.Time `gorm:"not null"`
}

func (Archive) TableName() string { return "database_archives" }

// ArchiveReasonPurge : copie prise avant la suppression des données d'une
// organisation suspendue.
const ArchiveReasonPurge = "suspension_purge"
