package domain

import (
	"errors"
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

// Organization : aussi la vue rendue par GET /organizations/:id.
type Organization struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	DisplayName string     `gorm:"not null" json:"display_name" description:"Nom affiché"`
	Kind        string     `gorm:"not null;default:'TEAM'" json:"kind" description:"PERSONAL (organisation personnelle) ou TEAM"`
	OwnerUserID *uuid.UUID `gorm:"type:uuid" json:"-"`
	// Country : pays du client (ISO 3166-1 alpha-2), "" si inconnu ; il
	// désigne la région suggérée à la création d'un projet.
	Country   string         `gorm:"type:varchar(2);not null;default:''" json:"country" description:"Pays (code ISO 3166-1 à deux lettres, ex. GA) ; vide si inconnu"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// ErrNotFound : organisation inexistante (ou supprimée).
var ErrNotFound = errors.New("organization not found")
