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
		i.containers, i.databases, i.builds, i.siteDomains, i.egressIPs, i.plans,
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

// egressIPs : IP de sortie dédiées attribuées à un projet vivant (ADR 0014).
func (i *Inventory) egressIPs(ctx context.Context, _ time.Time) ([]domain.Reservation, error) {
	var rows []domainRow
	err := i.DB.WithContext(ctx).Raw(`
		SELECT e.id, e.project_id, p.organization_id, e.address AS hostname
		FROM egress_addresses e JOIN projects p ON p.id = e.project_id AND p.deleted_at IS NULL`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory egress addresses: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Reservation{Kind: domain.KindEgressIP, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID,
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

// RunningWorkloads : conteneurs ni arrêtés ni supprimés d'une organisation
// (project : d'un seul projet).
func (i *Inventory) RunningWorkloads(ctx context.Context, org uuid.UUID, project *uuid.UUID) ([]domain.WorkloadRef, error) {
	q := `SELECT w.id, w.project_id, w.name FROM workloads w
		JOIN projects p ON p.id = w.project_id AND p.deleted_at IS NULL
		WHERE p.organization_id = ? AND w.status NOT IN ('STOPPED', 'DELETED')`
	args := []any{org}
	if project != nil {
		q += ` AND w.project_id = ?`
		args = append(args, *project)
	}
	var out []domain.WorkloadRef
	err := i.DB.WithContext(ctx).Raw(q+` ORDER BY w.created_at`, args...).Scan(&out).Error
	return out, err
}

// ProjectOrg : organisation d'un projet.
func (i *Inventory) ProjectOrg(ctx context.Context, project uuid.UUID) (uuid.UUID, error) {
	var rows []uuid.UUID
	err := i.DB.WithContext(ctx).Raw(`SELECT organization_id FROM projects WHERE id = ?`, project).Scan(&rows).Error
	if err != nil {
		// Erreur de lecture : remontée telle quelle (jamais « projet inconnu »,
		// qui ouvrirait la garde CanRun pendant une panne de la base).
		return uuid.Nil, fmt.Errorf("billing inventory: project %s: %w", project, err)
	}
	if len(rows) == 0 {
		return uuid.Nil, fmt.Errorf("billing inventory: project %s: %w", project, domain.ErrProjectUnknown)
	}
	return rows[0], nil
}

// plans : abonnement des organisations à un plan payant (dans la monnaie du
// compte). Rattaché à l'organisation, pas à un projet (ProjectID nul).
func (i *Inventory) plans(ctx context.Context, _ time.Time) ([]domain.Reservation, error) {
	var rows []struct {
		OrganizationID uuid.UUID
		PlanID         string
		Name           string
		Fee            int64
	}
	err := i.DB.WithContext(ctx).Raw(`
		SELECT a.organization_id, a.plan_id, p.name, COALESCE((p.fees->>a.currency)::bigint, 0) AS fee
		FROM billing_accounts a JOIN billing_plans p ON p.id = a.plan_id
		JOIN organizations o ON o.id = a.organization_id AND o.deleted_at IS NULL`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing inventory plans: %w", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		if r.Fee <= 0 {
			continue
		}
		out = append(out, domain.Reservation{Kind: domain.KindPlan, OrganizationID: r.OrganizationID, ResourceID: r.OrganizationID,
			ResourceName: r.Name, Size: r.PlanID, Monthly: domain.FromUnits(r.Fee)})
	}
	return out, nil
}

// ProjectUsages : ce que chaque projet de l'organisation réserve, selon les
// règles des quotas (conteneur arrêté ou en échec compris, un réplica au
// moins ; seul un conteneur supprimé ne compte plus).
func (i *Inventory) ProjectUsages(ctx context.Context, org uuid.UUID) ([]domain.ProjectUsage, error) {
	var projects []struct {
		ID   uuid.UUID
		Name string
	}
	if err := i.DB.WithContext(ctx).Raw(`SELECT id, name FROM projects WHERE organization_id = ? AND deleted_at IS NULL ORDER BY name`, org).
		Scan(&projects).Error; err != nil {
		return nil, err
	}
	var rows []struct {
		ProjectID             uuid.UUID
		Replicas              int
		CPULimit, MemoryLimit string
	}
	if err := i.DB.WithContext(ctx).Raw(`SELECT w.project_id, w.replicas, w.cpu_limit, w.memory_limit FROM workloads w
		JOIN projects p ON p.id = w.project_id WHERE p.organization_id = ? AND p.deleted_at IS NULL AND w.status <> 'DELETED'`, org).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	used := map[uuid.UUID]*domain.ProjectUsage{}
	out := make([]domain.ProjectUsage, len(projects))
	for k, p := range projects {
		out[k] = domain.ProjectUsage{ProjectID: p.ID, Name: p.Name}
		used[p.ID] = &out[k]
	}
	for _, r := range rows {
		u := used[r.ProjectID]
		if u == nil {
			continue
		}
		n := int64(max(r.Replicas, 1))
		u.CPUMilli += milliCPU(r.CPULimit) * n
		u.MemMi += mebibytes(r.MemoryLimit) * n
	}
	return out, nil
}

// LimitTargets : projets dont les limites diffèrent de leur cible (exception
// de l'équipe, sinon plan de l'organisation ; plan par défaut sans compte).
// org / plan : restreindre à une organisation, ou aux organisations d'un plan.
func (i *Inventory) LimitTargets(ctx context.Context, org *uuid.UUID, plan *string) ([]domain.LimitTarget, error) {
	q := `SELECT p.id AS project_id, p.organization_id, p.name,
			COALESCE(o.cpu_limit, pl.cpu_limit) AS cpu, COALESCE(o.memory_limit, pl.memory_limit) AS memory,
			COALESCE(o.storage_limit, pl.storage_limit) AS storage, o.project_id IS NOT NULL AS override
		FROM projects p
		LEFT JOIN billing_accounts a ON a.organization_id = p.organization_id
		JOIN billing_plans pl ON pl.id = COALESCE(a.plan_id, ?)
		LEFT JOIN billing_limit_overrides o ON o.project_id = p.id
		WHERE p.deleted_at IS NULL
		  AND (p.cpu_limit <> COALESCE(o.cpu_limit, pl.cpu_limit)
		    OR p.memory_limit <> COALESCE(o.memory_limit, pl.memory_limit)
		    OR p.storage_limit <> COALESCE(o.storage_limit, pl.storage_limit))`
	args := []any{domain.DefaultPlan}
	if org != nil {
		q += ` AND p.organization_id = ?`
		args = append(args, *org)
	}
	if plan != nil {
		q += ` AND COALESCE(a.plan_id, ?) = ? AND o.project_id IS NULL`
		args = append(args, domain.DefaultPlan, *plan)
	}
	var out []domain.LimitTarget
	err := i.DB.WithContext(ctx).Raw(q+` ORDER BY p.organization_id, p.name`, args...).Scan(&out).Error
	return out, err
}
