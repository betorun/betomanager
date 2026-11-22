package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	billingSvc "github.com/thekrauss/kubemanager/internal/modules/billing/service"
	databasesDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	databasesSvc "github.com/thekrauss/kubemanager/internal/modules/databases/service"
	sitesSvc "github.com/thekrauss/kubemanager/internal/modules/sites/service"
	workloadsSvc "github.com/thekrauss/kubemanager/internal/modules/workloads/service"
)

// startSuspensionPurge : suppression des données des organisations
// suspendues trop longtemps (ADR 0012). Tourne dans le processus API : la
// copie finale d'une base arrive par une URL signée de l'API (comme celle de
// la remise à zéro), attendue en mémoire. Une seule instance de l'API.
func (a *App) startSuspensionPurge(ctx context.Context) {
	if a.DB == nil || a.Services == nil || a.Services.Workload == nil || a.Controllers == nil {
		return
	}
	billing := a.billingService()
	if billing == nil {
		return
	}
	p := &orgPurger{a: a, workloads: a.Services.Workload, sites: a.Services.Site}
	if a.Controllers.Databases != nil {
		p.databases = a.Controllers.Databases.Service
	}
	billing.Purger = p
	if n := a.alertNotifier(); n != nil { // pas d'interface non nulle autour d'un nil
		billing.PurgeAlerts = n
	}
	a.goWith(ctx, "billing-purge", func(ctx context.Context) { billing.RunPurge(ctx, billingSvc.DefaultPurgeEvery) })
	if p.databases != nil {
		a.goWith(ctx, "database-archives-expiry", p.expireArchives)
	}
}

// orgPurger : supprime conteneurs (et disques), bases (copie finale d'abord)
// et sites d'une organisation. Les suppressions passent par les services
// habituels (workflows Temporal) : « lancée » suffit.
type orgPurger struct {
	a         *App
	workloads workloadsSvc.IWorkloadService
	databases *databasesSvc.Service
	sites     sitesSvc.ISiteService // nil : sites désactivés
}

type purgeRef struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
}

// errPurgeIncomplete : une ressource au moins n'a pas pu être supprimée ; la
// purge reprend au passage suivant.
var errPurgeIncomplete = errors.New("some resources of the organization were not deleted")

func (p *orgPurger) list(ctx context.Context, query string, org uuid.UUID) ([]purgeRef, error) {
	var out []purgeRef
	err := p.a.DB.WithContext(ctx).Raw(query, org).Scan(&out).Error
	return out, err
}

const (
	purgeWorkloadsSQL = `SELECT w.id, w.project_id FROM workloads w JOIN projects p ON p.id = w.project_id
		WHERE p.organization_id = ? AND w.status NOT IN ('DELETING', 'DELETED')`
	purgeDatabasesSQL = `SELECT d.id, d.project_id FROM databases d JOIN projects p ON p.id = d.project_id
		WHERE p.organization_id = ? AND d.status <> 'DELETING'`
	purgeSitesSQL = `SELECT s.id, s.project_id FROM sites s JOIN projects p ON p.id = s.project_id
		WHERE p.organization_id = ? AND s.status <> 'DELETING'`
)

func (p *orgPurger) PurgeOrganization(ctx context.Context, org uuid.UUID, archiveUntil time.Time) error {
	log := p.a.Logger.With("component", "billing-purge", "organization", org)
	failed := 0
	// Conteneurs d'abord : plus personne n'écrit dans les bases.
	ws, err := p.list(ctx, purgeWorkloadsSQL, org)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if err := p.workloads.Delete(ctx, w.ID); err != nil {
			log.Warnw("purge: container not deleted", "workload", w.ID, "error", err)
			failed++
		}
	}
	dbs, err := p.list(ctx, purgeDatabasesSQL, org)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if p.databases == nil {
			failed++
			continue
		}
		if err := p.databases.ArchiveAndDelete(ctx, org, d.ProjectID, d.ID, databasesDomain.ArchiveReasonPurge, archiveUntil); err != nil {
			log.Warnw("purge: database not deleted", "database", d.ID, "error", err)
			failed++
		}
	}
	if p.sites != nil {
		ss, err := p.list(ctx, purgeSitesSQL, org)
		if err != nil {
			return err
		}
		for _, s := range ss {
			if err := p.sites.DeleteSite(ctx, s.ProjectID, s.ID); err != nil {
				log.Warnw("purge: site not deleted", "site", s.ID, "error", err)
				failed++
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%w (%d)", errPurgeIncomplete, failed)
	}
	log.Infow("purge: resources deleted", "containers", len(ws), "databases", len(dbs))
	return nil
}

// expireArchives : copies finales arrivées à échéance, effacées chaque jour.
func (p *orgPurger) expireArchives(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		if n, err := p.databases.PurgeExpiredArchives(ctx); err != nil {
			p.a.Logger.Warnw("databases: expired final copies not deleted", "error", err)
		} else if n > 0 {
			p.a.Logger.Infow("databases: expired final copies deleted", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
