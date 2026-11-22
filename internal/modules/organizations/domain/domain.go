package domain

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	KindPersonal = "PERSONAL"
	KindTeam     = "TEAM"
)

// defaultPersonalOrgName : nom de l'organisation personnelle créée sans nom.
const DefaultPersonalOrgName = "Mon organisation"

type Organization struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	DisplayName string         `gorm:"not null" json:"display_name"`
	Kind        string         `gorm:"not null;default:'TEAM'" json:"kind"`
	OwnerUserID *uuid.UUID     `gorm:"type:uuid" json:"-"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}
