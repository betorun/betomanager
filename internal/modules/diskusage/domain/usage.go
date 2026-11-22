// Package diskusage : usage réel des disques des projets.
//
// Le stockage local-path ne plafonne pas la taille demandée : un disque de
// 1 Gi peut grossir jusqu'à remplir le serveur. Ce module mesure l'usage
// réel de chaque disque (PVC) toutes les heures, par un Job en lecture seule
// dans l'espace du projet, et applique la règle :
//   - 80 % : alerte aux propriétaires et éditeurs ;
//   - 100 % : alerte « disque plein » ;
//   - 120 % : le conteneur ou la base est arrêté (données gardées) et une
//     alerte part. Relancé par le client, il a un délai de grâce pour faire
//     de la place.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Sortes de disque.
const (
	KindWorkload = "workload" // disque d'un conteneur (services compris)
	KindDatabase = "database" // disque d'une base PostgreSQL (CloudNativePG)
)

// Seuils (pourcentage de la taille payée).
const (
	LevelOK      = 0
	LevelWarning = 80
	LevelFull    = 100
	LevelStop    = 120
)

// StopGrace : après un arrêt pour disque plein, un conteneur ou une base relancé par le
// client n'est pas arrêté de nouveau avant ce délai (le temps de faire de la
// place).
const StopGrace = 6 * time.Hour

// Usage : dernière mesure d'un disque (table disk_usage).
type Usage struct {
	Namespace     string    `gorm:"primaryKey" json:"-"`
	PVC           string    `gorm:"column:pvc;primaryKey" json:"-"`
	ProjectID     uuid.UUID `gorm:"type:uuid;not null" json:"-"`
	Kind          string    `gorm:"not null" json:"kind"`
	TargetID      uuid.UUID `gorm:"type:uuid;not null" json:"target_id"`
	TargetName    string    `gorm:"not null" json:"target_name"`
	CapacityBytes int64     `gorm:"not null" json:"capacity_bytes"`
	UsedBytes     int64     `gorm:"not null" json:"used_bytes"`
	Level         int       `gorm:"not null" json:"level"`
	MeasuredAt    time.Time `gorm:"not null" json:"measured_at"`
	// StoppedAt : dernier arrêt pour disque plein (absent sinon).
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}

func (Usage) TableName() string { return "disk_usage" }

// LevelOf : seuil franchi pour cet usage.
func LevelOf(used, capacity int64) int {
	if capacity <= 0 {
		return LevelOK
	}
	pct := used * 100 / capacity
	switch {
	case pct >= LevelStop:
		return LevelStop
	case pct >= LevelFull:
		return LevelFull
	case pct >= LevelWarning:
		return LevelWarning
	}
	return LevelOK
}

// UsageList : disques d'un projet (réponse de l'API).
type UsageList struct {
	Disks []Usage `json:"disks"`
}

// Disk : un disque d'un projet et ce qu'il porte.
type Disk struct {
	Namespace string
	PVC       string
	Kind      string // KindWorkload ou KindDatabase
	// Name : nom du conteneur (release Helm) ou de la base.
	Name          string
	CapacityBytes int64
}

// Target : conteneur ou base qui porte un disque.
type Target struct {
	ID     uuid.UUID
	Status string
}

// Project : un projet actif et son espace.
type Project struct {
	ID        uuid.UUID
	Namespace string
}
