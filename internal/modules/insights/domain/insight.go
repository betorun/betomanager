// Package domain : « L'assistant a remarqué » — constats sur un projet.
//
// Un constat (Insight) signale ce qui mérite l'attention de la personne : un
// conteneur qui redémarre en boucle, un build en échec, un budget presque
// atteint, un disque presque plein, un domaine jamais vérifié, une base
// arrêtée, un service en panne. Chacun est tiré de l'état réel lu par les
// services des modules concernés ; une source illisible est omise, jamais
// devinée. La console les affiche ; le Copilot les lit (get_project_insights).
package domain

import (
	"sort"
	"time"
)

// Sortes de constat (contrat de la console : ne pas renommer).
const (
	KindWorkloadCrashloop = "workload_crashloop"
	KindWorkloadOOM       = "workload_oom"
	KindWorkloadImagePull = "workload_image_pull"
	KindWorkloadFailed    = "workload_failed"
	KindBuildFailed       = "build_failed"
	KindBudgetHigh        = "budget_high"
	KindBudgetStopped     = "budget_stopped"
	KindDiskHigh          = "disk_high"
	KindDomainUnverified  = "domain_unverified"
	KindDatabaseStopped   = "database_stopped"
	KindAddonUnhealthy    = "addon_unhealthy"
)

// Gravités.
const (
	SeverityDanger = "danger"
	SeverityWarn   = "warn"
	SeverityInfo   = "info"
)

// Types de ressource.
const (
	ResourceWorkload = "workload"
	ResourceBuild    = "build"
	ResourceDatabase = "database"
	ResourceAddon    = "addon"
	ResourceDomain   = "domain"
	ResourceProject  = "project"
)

// Seuils.
const (
	// MaxInsights : constats rendus au plus.
	MaxInsights = 10
	// BudgetHighPercent : part du budget consommée qui fait un constat.
	BudgetHighPercent = 80
	// DiskHighPercent : remplissage d'un disque qui fait un constat.
	DiskHighPercent = 85
	// DomainGrace : un domaine non vérifié n'est signalé qu'après ce délai
	// (le temps de poser ses enregistrements DNS).
	DomainGrace = time.Hour
)

// Resource : ressource visée par un constat.
type Resource struct {
	Type string `json:"type" description:"Type de ressource (conteneur, build, base de données, service, domaine ou projet)"`
	ID   string `json:"id" description:"Identifiant de la ressource"`
	Name string `json:"name" description:"Nom de la ressource"`
}

// Insight : un constat.
type Insight struct {
	// Key : stable d'une lecture à l'autre (ex. workload_crashloop:<id>).
	Key      string   `json:"key" description:"Clé stable du constat (sorte:identifiant)"`
	Kind     string   `json:"kind" description:"Sorte de constat (workload_crashloop, workload_oom, workload_image_pull, workload_failed, build_failed, budget_high, budget_stopped, disk_high, domain_unverified, database_stopped, addon_unhealthy)"`
	Severity string   `json:"severity" description:"Gravité : danger, warn ou info"`
	Resource Resource `json:"resource" description:"Ressource concernée"`
	// Params : valeurs utiles à l'affichage (restarts, percent, hostname,
	// workload_id, status…), toutes en texte.
	Params map[string]string `json:"params" description:"Valeurs du constat (restarts, percent, hostname, workload_id, status…)"`
	Since  *time.Time        `json:"since,omitempty" description:"Depuis quand (si connu)"`
}

// InsightList : constats d'un projet.
type InsightList struct {
	GeneratedAt time.Time `json:"generated_at" description:"Instant du calcul (le résultat est gardé 60 s)"`
	Insights    []Insight `json:"insights" description:"Constats, du plus grave au moins grave puis du plus récent, 10 au plus"`
}

// ProjectPath : projet visé.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

func severityRank(s string) int {
	switch s {
	case SeverityDanger:
		return 0
	case SeverityWarn:
		return 1
	}
	return 2
}

// Rank : danger, puis warn, puis info ; à gravité égale le plus récent
// d'abord (sans date : après), puis par clé (ordre stable) ; MaxInsights au plus.
func Rank(in []Insight) []Insight {
	out := append([]Insight{}, in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra < rb
		}
		switch {
		case a.Since != nil && b.Since != nil && !a.Since.Equal(*b.Since):
			return a.Since.After(*b.Since)
		case a.Since != nil && b.Since == nil:
			return true
		case a.Since == nil && b.Since != nil:
			return false
		}
		return a.Key < b.Key
	})
	if len(out) > MaxInsights {
		out = out[:MaxInsights]
	}
	return out
}
