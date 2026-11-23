// Package service : calcul des constats d'un projet (« L'assistant a
// remarqué »), à partir des services des modules qui possèdent les données.
package service

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/insights/domain"
)

// CacheTTL : durée de vie d'un résultat (par projet et par droits).
const CacheTTL = 60 * time.Second

// restartsTimeout : lecture en direct des redémarrages d'un conteneur.
const restartsTimeout = 3 * time.Second

// Access : sources que la personne a le droit de lire dans le projet. Une
// source non accordée n'est pas lue : ses constats n'existent pas pour elle.
// Les disques suivent la permission de la route (lecture du projet).
type Access struct {
	Workloads       bool // workloads.workloads.list
	Builds          bool // workloads.builds.list
	Budget          bool // billing.accounts.get
	WorkloadDomains bool // workloads.workloads.list + workloads.domains.list
	SiteDomains     bool // sites.sites.list + sites.domains.list
	Databases       bool // databases.databases.list
	Addons          bool // addons.instances.list
}

// Key : empreinte des droits, pour la clé du cache (deux personnes aux
// droits différents ne partagent jamais un résultat).
func (a Access) Key() string {
	bits := 0
	for i, on := range []bool{a.Workloads, a.Builds, a.Budget, a.WorkloadDomains, a.SiteDomains, a.Databases, a.Addons} {
		if on {
			bits |= 1 << i
		}
	}
	return strconv.FormatInt(int64(bits), 16)
}

// Service : constats d'un projet. Toute source est facultative (nil : omise).
type Service struct {
	Workloads       Workloads
	Builds          Builds
	Budgets         Budgets
	Disks           Disks
	WorkloadDomains WorkloadDomains
	Sites           Sites
	Databases       Databases
	Addons          Addons
	Projects        Projects
	Cache           Cache
	Logger          Logger
	Now             func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) warn(source string, project uuid.UUID, err error) {
	if s.Logger != nil {
		s.Logger.Warnw("insights: source omitted", "source", source, "project", project, "error", err)
	}
}

// cacheKey : un résultat par projet ET par ensemble de droits.
func cacheKey(project uuid.UUID, a Access) string {
	return "betorun:insights:" + project.String() + ":" + a.Key()
}

// List : constats du projet, gardés CacheTTL. Le cache est une optimisation :
// illisible ou injoignable, le calcul est refait.
func (s *Service) List(ctx context.Context, project uuid.UUID, access Access) (*domain.InsightList, error) {
	key := cacheKey(project, access)
	if s.Cache != nil {
		if raw, ok, err := s.Cache.Get(ctx, key); err == nil && ok {
			var out domain.InsightList
			if json.Unmarshal(raw, &out) == nil {
				return &out, nil
			}
		} else if err != nil {
			s.warn("cache", project, err)
		}
	}
	out := &domain.InsightList{GeneratedAt: s.now().Truncate(time.Second), Insights: domain.Rank(s.collect(ctx, project, access))}
	if s.Cache != nil {
		if raw, err := json.Marshal(out); err == nil {
			if err := s.Cache.Set(ctx, key, raw, CacheTTL); err != nil {
				s.warn("cache", project, err)
			}
		}
	}
	return out, nil
}

func (s *Service) collect(ctx context.Context, project uuid.UUID, a Access) []domain.Insight {
	var out []domain.Insight
	var workloads []WorkloadState
	if s.Workloads != nil && (a.Workloads || a.WorkloadDomains) {
		var err error
		if workloads, err = s.Workloads.List(ctx, project); err != nil {
			s.warn("workloads", project, err)
			workloads = nil
		}
	}
	if a.Workloads {
		out = append(out, s.workloadInsights(ctx, workloads)...)
	}
	if a.Builds && s.Builds != nil {
		if builds, err := s.Builds.List(ctx, project); err != nil {
			s.warn("builds", project, err)
		} else {
			out = append(out, buildInsights(builds)...)
		}
	}
	if a.Budget && s.Budgets != nil {
		out = append(out, s.budgetInsights(ctx, project)...)
	}
	if s.Disks != nil {
		if disks, err := s.Disks.List(ctx, project); err != nil {
			s.warn("disks", project, err)
		} else {
			out = append(out, diskInsights(disks)...)
		}
	}
	now := s.now()
	if a.WorkloadDomains && s.WorkloadDomains != nil {
		for _, w := range workloads {
			ds, err := s.WorkloadDomains.List(ctx, w.ID)
			if err != nil {
				s.warn("workload_domains", project, err)
				continue
			}
			out = append(out, domainInsights(ds, now, "workload_id", w.ID)...)
		}
	}
	if a.SiteDomains && s.Sites != nil {
		if sites, err := s.Sites.List(ctx, project); err != nil {
			s.warn("sites", project, err)
		} else {
			for _, site := range sites {
				ds, err := s.Sites.Domains(ctx, site.ID)
				if err != nil {
					s.warn("site_domains", project, err)
					continue
				}
				out = append(out, domainInsights(ds, now, "site_id", site.ID)...)
			}
		}
	}
	if a.Databases && s.Databases != nil {
		if dbs, err := s.Databases.List(ctx, project); err != nil {
			s.warn("databases", project, err)
		} else {
			out = append(out, databaseInsights(dbs)...)
		}
	}
	if a.Addons && s.Addons != nil {
		if addons, err := s.Addons.List(ctx, project); err != nil {
			s.warn("addons", project, err)
		} else {
			out = append(out, addonInsights(addons)...)
		}
	}
	return out
}

// Statuts de conteneur (colonne workloads.status, voir utils/constants.go).
const (
	statusCrashLoop       = "CRASHLOOP"
	statusOOMKilled       = "OOM_KILLED"
	statusImagePullFailed = "IMAGE_PULL_FAILED"
	statusFailed          = "FAILED"
	statusConfigError     = "CONFIG_ERROR"
	statusEvicted         = "EVICTED"
	statusRunFailed       = "RUN_FAILED"
)

func insight(kind, severity, resType, id, name string, params map[string]string, since *time.Time) domain.Insight {
	if params == nil {
		params = map[string]string{}
	}
	return domain.Insight{Key: kind + ":" + id, Kind: kind, Severity: severity,
		Resource: domain.Resource{Type: resType, ID: id, Name: name}, Params: params, Since: since}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// workloadInsights : conteneurs du client dans un statut d'échec (les
// porteurs de services sont couverts par addon_unhealthy).
func (s *Service) workloadInsights(ctx context.Context, ws []WorkloadState) []domain.Insight {
	var out []domain.Insight
	for _, w := range ws {
		if w.ManagedBy != "" {
			continue
		}
		params := map[string]string{"status": w.Status}
		id, since := w.ID.String(), timePtr(w.UpdatedAt)
		switch w.Status {
		case statusCrashLoop:
			if s.Workloads != nil {
				rctx, cancel := context.WithTimeout(ctx, restartsTimeout)
				if n, err := s.Workloads.Restarts(rctx, w.ID); err == nil && n > 0 {
					params["restarts"] = strconv.Itoa(n)
				}
				cancel()
			}
			out = append(out, insight(domain.KindWorkloadCrashloop, domain.SeverityDanger, domain.ResourceWorkload, id, w.Name, params, since))
		case statusOOMKilled:
			out = append(out, insight(domain.KindWorkloadOOM, domain.SeverityDanger, domain.ResourceWorkload, id, w.Name, params, since))
		case statusImagePullFailed:
			out = append(out, insight(domain.KindWorkloadImagePull, domain.SeverityDanger, domain.ResourceWorkload, id, w.Name, params, since))
		case statusFailed, statusConfigError:
			out = append(out, insight(domain.KindWorkloadFailed, domain.SeverityDanger, domain.ResourceWorkload, id, w.Name, params, since))
		case statusEvicted, statusRunFailed:
			out = append(out, insight(domain.KindWorkloadFailed, domain.SeverityWarn, domain.ResourceWorkload, id, w.Name, params, since))
		}
	}
	return out
}

// Statuts d'un build (builds/domain).
const (
	buildSucceeded = "SUCCEEDED"
	buildFailed    = "FAILED"
)

// buildInsights : pour chaque conteneur, son dernier build terminé est un
// échec plus récent que son dernier succès.
func buildInsights(builds []BuildState) []domain.Insight {
	type last struct{ failed, succeeded *BuildState }
	byWorkload := map[uuid.UUID]*last{}
	var order []uuid.UUID
	at := func(b *BuildState) time.Time {
		if b.FinishedAt != nil {
			return *b.FinishedAt
		}
		return b.CreatedAt
	}
	for i := range builds {
		b := &builds[i]
		if b.WorkloadID == nil || (b.Status != buildFailed && b.Status != buildSucceeded) {
			continue
		}
		l, ok := byWorkload[*b.WorkloadID]
		if !ok {
			l = &last{}
			byWorkload[*b.WorkloadID] = l
			order = append(order, *b.WorkloadID)
		}
		if b.Status == buildFailed && (l.failed == nil || at(b).After(at(l.failed))) {
			l.failed = b
		}
		if b.Status == buildSucceeded && (l.succeeded == nil || at(b).After(at(l.succeeded))) {
			l.succeeded = b
		}
	}
	var out []domain.Insight
	for _, wid := range order {
		l := byWorkload[wid]
		if l.failed == nil || (l.succeeded != nil && !at(l.failed).After(at(l.succeeded))) {
			continue
		}
		params := map[string]string{"workload_id": wid.String()}
		if l.failed.Failure != "" {
			params["failure"] = l.failed.Failure
		}
		out = append(out, insight(domain.KindBuildFailed, domain.SeverityWarn, domain.ResourceBuild,
			l.failed.ID.String(), l.failed.Name, params, timePtr(at(l.failed))))
	}
	return out
}

func (s *Service) budgetInsights(ctx context.Context, project uuid.UUID) []domain.Insight {
	b, err := s.Budgets.Budget(ctx, project)
	if err != nil {
		s.warn("budget", project, err)
		return nil
	}
	if b == nil || b.Limit <= 0 {
		return nil
	}
	name := ""
	if s.Projects != nil {
		if n, err := s.Projects.Name(ctx, project); err == nil {
			name = n
		} else {
			s.warn("project", project, err)
		}
	}
	id := project.String()
	percent := b.Spent * 100 / b.Limit
	params := map[string]string{"percent": strconv.FormatInt(percent, 10)}
	if b.Stopped {
		return []domain.Insight{insight(domain.KindBudgetStopped, domain.SeverityDanger, domain.ResourceProject, id, name, params, nil)}
	}
	if percent < domain.BudgetHighPercent {
		return nil
	}
	severity := domain.SeverityWarn
	if percent >= 100 {
		severity = domain.SeverityDanger
	}
	return []domain.Insight{insight(domain.KindBudgetHigh, severity, domain.ResourceProject, id, name, params, nil)}
}

// diskInsights : disques remplis à DiskHighPercent ou plus.
func diskInsights(disks []DiskState) []domain.Insight {
	var out []domain.Insight
	for _, d := range disks {
		if d.Capacity <= 0 {
			continue
		}
		percent := d.Used * 100 / d.Capacity
		if percent < domain.DiskHighPercent {
			continue
		}
		severity := domain.SeverityWarn
		if percent >= 100 {
			severity = domain.SeverityDanger
		}
		resType := domain.ResourceWorkload
		if d.Kind == "database" {
			resType = domain.ResourceDatabase
		}
		out = append(out, insight(domain.KindDiskHigh, severity, resType, d.TargetID.String(), d.TargetName,
			map[string]string{"percent": strconv.FormatInt(percent, 10)}, timePtr(d.MeasuredAt)))
	}
	return out
}

// domainInsights : domaines non vérifiés depuis plus de DomainGrace.
func domainInsights(ds []DomainState, now time.Time, owner string, ownerID uuid.UUID) []domain.Insight {
	var out []domain.Insight
	for _, d := range ds {
		if d.Verified || d.CreatedAt.IsZero() || now.Sub(d.CreatedAt) <= domain.DomainGrace {
			continue
		}
		out = append(out, insight(domain.KindDomainUnverified, domain.SeverityWarn, domain.ResourceDomain, d.ID.String(), d.Hostname,
			map[string]string{"hostname": d.Hostname, owner: ownerID.String()}, timePtr(d.CreatedAt)))
	}
	return out
}

// databaseInsights : bases arrêtées (grave si des conteneurs en dépendent).
func databaseInsights(dbs []DatabaseState) []domain.Insight {
	var out []domain.Insight
	for _, d := range dbs {
		if d.Status != "STOPPED" {
			continue
		}
		severity := domain.SeverityInfo
		if d.Attached > 0 {
			severity = domain.SeverityDanger
		}
		out = append(out, insight(domain.KindDatabaseStopped, severity, domain.ResourceDatabase, d.ID.String(), d.Name,
			map[string]string{"status": d.Status, "attached": strconv.Itoa(d.Attached)}, timePtr(d.UpdatedAt)))
	}
	return out
}

// unhealthyAddon : statuts d'un porteur de service qui font un constat.
var unhealthyAddon = map[string]bool{
	statusCrashLoop: true, statusOOMKilled: true, statusImagePullFailed: true, statusFailed: true,
	statusConfigError: true, statusEvicted: true, "DEGRADED": true,
}

func addonInsights(addons []AddonState) []domain.Insight {
	var out []domain.Insight
	for _, a := range addons {
		if !unhealthyAddon[a.Status] {
			continue
		}
		out = append(out, insight(domain.KindAddonUnhealthy, domain.SeverityDanger, domain.ResourceAddon, a.ID, a.Name,
			map[string]string{"status": a.Status}, nil))
	}
	return out
}
