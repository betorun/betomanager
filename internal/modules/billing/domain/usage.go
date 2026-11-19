package domain

import (
	"time"

	"github.com/google/uuid"
)

// ResourceKind : nature d'une ressource relevée.
type ResourceKind string

const (
	KindContainer  ResourceKind = "container"
	KindDatabase   ResourceKind = "database"
	KindBuild      ResourceKind = "build"
	KindSiteDomain ResourceKind = "site_domain"
)

// Reservation : ce qu'une ressource réserve au moment du relevé (lu par
// l'inventaire, en lecture seule, dans les tables des autres modules).
type Reservation struct {
	Kind           ResourceKind
	OrganizationID uuid.UUID
	ProjectID      uuid.UUID
	ResourceID     uuid.UUID
	ResourceName   string
	// Conteneur : limites d'un réplica et nombre de réplicas en marche
	// (0 : arrêté, seul son disque compte).
	CPUMilli int64
	MemMi    int64
	Replicas int
	// DiskGi : disque réservé (conteneur ou base), arrondi au Go supérieur.
	DiskGi int64
	// BuildMinutes et FinishedAt : build terminé (minutes commencées).
	BuildMinutes int64
	FinishedAt   time.Time
	// Monthly : abonnement mensuel du plan (KindPlan) ; Size : son identifiant.
	Monthly Milli
	Size    string
}

// Usage : une ligne par (compteur, ressource, heure). Le relevé garde le
// maximum vu dans l'heure ; la clé rend tout relevé idempotent.
type Usage struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Hour           time.Time `gorm:"not null"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null"`
	ProjectID      uuid.UUID `gorm:"type:uuid;not null"`
	Meter          Meter     `gorm:"type:varchar(30);not null"`
	ResourceID     uuid.UUID `gorm:"type:uuid;not null"`
	ResourceName   string    `gorm:"not null"`
	// Size : taille du conteneur (vide pour les autres compteurs).
	Size string `gorm:"not null;default:''"`
	// Quantity : réplicas, Go ou minutes selon le compteur.
	Quantity int64 `gorm:"not null"`
	// UnitPrice : prix d'une unité pour l'heure (pour une minute de build).
	UnitPrice      Milli  `gorm:"not null"`
	Amount         Milli  `gorm:"not null"`
	Currency       string `gorm:"type:varchar(3);not null"`
	CatalogVersion string `gorm:"not null"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Usage) TableName() string { return "billing_usage" }

// Price : lignes d'usage d'une réservation pour l'heure donnée. Un build est
// compté dans l'heure où il s'est terminé.
func (c Catalog) Price(r Reservation, hour time.Time) []Usage {
	line := func(m Meter, size string, qty int64, unit Milli, at time.Time) Usage {
		return Usage{Hour: at, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, Meter: m,
			ResourceID: r.ResourceID, ResourceName: r.ResourceName, Size: size, Quantity: qty,
			UnitPrice: unit, Amount: unit * Milli(qty), Currency: c.Currency, CatalogVersion: c.Version}
	}
	var out []Usage
	switch r.Kind {
	case KindContainer:
		if r.Replicas > 0 {
			size, monthly := c.SizeFor(r.CPUMilli, r.MemMi)
			out = append(out, line(MeterCompute, size, int64(r.Replicas), PerHour(monthly), hour))
		}
		if r.DiskGi > 0 {
			out = append(out, line(MeterDisk, "", r.DiskGi, PerHour(c.DiskGiMonthly), hour))
		}
	case KindDatabase:
		out = append(out, line(MeterDatabase, "", 1, PerHour(c.DatabaseMonthly), hour))
		if r.DiskGi > 0 {
			out = append(out, line(MeterDatabaseDisk, "", r.DiskGi, PerHour(c.DiskGiMonthly), hour))
		}
	case KindBuild:
		if r.BuildMinutes > 0 && !r.FinishedAt.IsZero() {
			out = append(out, line(MeterBuildMinutes, "", r.BuildMinutes, c.BuildMinute, r.FinishedAt.UTC().Truncate(time.Hour)))
		}
	case KindSiteDomain:
		out = append(out, line(MeterSiteDomain, "", 1, PerHour(c.SiteDomain), hour))
	case KindPlan:
		if r.Monthly > 0 {
			out = append(out, line(MeterPlan, r.Size, 1, PerHour(r.Monthly), hour))
		}
	}
	return out
}
