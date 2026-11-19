// Package inventory : ce que chaque ressource réserve, lu en lecture seule
// dans les tables des autres modules (conteneurs, bases, builds, sites).
// C'est le seul endroit où la facturation connaît leur schéma ; elle n'écrit
// jamais chez eux.
package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
)

// Inventory : relevé SQL.
type Inventory struct{ DB *gorm.DB }

// New : inventaire sur la base de la plateforme.
func New(db *gorm.DB) *Inventory { return &Inventory{DB: db} }

// Reservations : tout ce qui est réservé maintenant, plus les builds terminés
// depuis since (comptés dans l'heure de leur fin). Projets supprimés exclus.
func (i *Inventory) Reservations(ctx context.Context, since time.Time) ([]domain.Reservation, error) {
	var out []domain.Reservation
	for _, read := range []func(context.Context, time.Time) ([]domain.Reservation, error){
		i.containers, i.databases, i.builds, i.siteDomains,
	} {
		rs, err := read(ctx, since)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

type containerRow struct {
	ID, ProjectID, OrganizationID uuid.UUID
	Name, Status                  string
	Replicas                      int
	CPULimit, MemoryLimit         string
	PersistenceEnabled            bool
	StorageSize                   string
}

// containers : un conteneur supprimé ne compte plus ; arrêté, seul son disque
// compte (0 réplica) ; en échec, il compte (ses pods tournent, comme pour les
// quotas).
func (i *Inventory) containers(ctx context.Context, _ time.Time) ([]domain.Reservation, error) {
	var rows []containerRow
	err := i.DB.WithContext(ctx).Raw(`
		SELECT w.id, w.project_id, p.organization_id, w.name, w.status, w.replicas,
		       w.cpu_limit, w.memory_limit, w.persistence_enabled, w.storage_size
		FROM workloads w JOIN projects p ON p.id = w.project_id AND p.deleted_at IS NULL
		WHERE w.status <> 'DELETED'`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory containers: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		replicas := max(r.Replicas, 1)
		if r.Status == "STOPPED" {
			replicas = 0
		}
		res := domain.Reservation{Kind: domain.KindContainer, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID,
			ResourceID: r.ID, ResourceName: r.Name, CPUMilli: milliCPU(r.CPULimit), MemMi: mebibytes(r.MemoryLimit), Replicas: replicas}
		if r.PersistenceEnabled {
			res.DiskGi = gibibytes(r.StorageSize)
		}
		out = append(out, res)
	}
	return out, nil
}

type databaseRow struct {
	ID, ProjectID, OrganizationID uuid.UUID
	Name, StorageSize             string
}

func (i *Inventory) databases(ctx context.Context, _ time.Time) ([]domain.Reservation, error) {
	var rows []databaseRow
	err := i.DB.WithContext(ctx).Raw(`
		SELECT d.id, d.project_id, p.organization_id, d.name, d.storage_size
		FROM databases d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory databases: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Reservation{Kind: domain.KindDatabase, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID,
			ResourceID: r.ID, ResourceName: r.Name, DiskGi: gibibytes(r.StorageSize)})
	}
	return out, nil
}

type buildRow struct {
	ID, ProjectID, OrganizationID uuid.UUID
	Name                          string
	StartedAt, FinishedAt         time.Time
}

// builds : minutes commencées des builds terminés depuis since. Une panne de
// la plateforme (échec INTERNAL) n'est pas facturée.
func (i *Inventory) builds(ctx context.Context, since time.Time) ([]domain.Reservation, error) {
	var rows []buildRow
	err := i.DB.WithContext(ctx).Raw(`
		SELECT b.id, b.project_id, p.organization_id, b.name, b.started_at, b.finished_at
		FROM builds b JOIN projects p ON p.id = b.project_id
		WHERE b.started_at IS NOT NULL AND b.finished_at >= ? AND b.failure <> 'INTERNAL'`, since).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory builds: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		d := r.FinishedAt.Sub(r.StartedAt)
		if d <= 0 {
			continue
		}
		out = append(out, domain.Reservation{Kind: domain.KindBuild, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID,
			ResourceID: r.ID, ResourceName: r.Name, BuildMinutes: int64((d + time.Minute - 1) / time.Minute), FinishedAt: r.FinishedAt})
	}
	return out, nil
}

type domainRow struct {
	ID, ProjectID, OrganizationID uuid.UUID
	Hostname                      string
}

// siteDomains : domaines personnalisés vérifiés (servis) des sites.
func (i *Inventory) siteDomains(ctx context.Context, _ time.Time) ([]domain.Reservation, error) {
	var rows []domainRow
	err := i.DB.WithContext(ctx).Raw(`
		SELECT sd.id, s.project_id, p.organization_id, sd.hostname
		FROM site_domains sd JOIN sites s ON s.id = sd.site_id
		JOIN projects p ON p.id = s.project_id AND p.deleted_at IS NULL
		WHERE sd.verified`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory site domains: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Reservation{Kind: domain.KindSiteDomain, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID,
			ResourceID: r.ID, ResourceName: r.Hostname})
	}
	return out, nil
}

// ProjectNames : nom des projets (y compris supprimés : leur usage reste dû).
func (i *Inventory) ProjectNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID   uuid.UUID
		Name string
	}
	if err := i.DB.WithContext(ctx).Raw(`SELECT id, name FROM projects WHERE id IN ?`, ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out, nil
}

func milliCPU(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func mebibytes(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return (q.Value() + (1<<20 - 1)) >> 20
}

// gibibytes : Go commencés.
func gibibytes(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return (q.Value() + (1<<30 - 1)) >> 30
}
