package domain

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidMonth    = errors.New("invalid month: use YYYY-MM")
	ErrUnknownCurrency = errors.New("this currency is not offered")
)

// Total : usage cumulé d'une ressource sur la période (agrégé par le dépôt).
type Total struct {
	ProjectID    uuid.UUID
	Meter        Meter
	ResourceID   uuid.UUID
	ResourceName string
	// Size et Quantity : valeurs de la dernière heure relevée.
	Size     string
	Quantity int64
	Hours    int64
	// Sum : somme des lignes ; Peak : ligne horaire la plus chère.
	Sum  Milli
	Peak Milli
}

// Cost : coût d'une ressource, plafonné au mois.
func (t Total) Cost() Milli {
	if !t.Meter.Hourly() {
		return t.Sum
	}
	return minMilli(t.Sum, t.Peak*HoursPerMonth)
}

// EstimateLine : une ressource dans l'estimation.
type EstimateLine struct {
	ProjectID    string `json:"project_id"`
	Meter        Meter  `json:"meter"`
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name"`
	Size         string `json:"size,omitempty"`
	Quantity     int64  `json:"quantity"`
	Hours        int64  `json:"hours"`
	Amount       int64  `json:"amount"`
}

// ProjectCost : total d'un projet.
type ProjectCost struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
}

// FreeTierUsed : part de l'offre gratuite déduite.
type FreeTierUsed struct {
	Compute      int64 `json:"compute"`
	Database     int64 `json:"database"`
	BuildMinutes int64 `json:"build_minutes"`
	Total        int64 `json:"total"`
}

// Estimate : coût estimé d'une organisation pour un mois. Montants en unités
// de la monnaie (FCFA), arrondis. Rien n'est débité (phase 2).
type Estimate struct {
	Month    string         `json:"month"`
	Currency string         `json:"currency"`
	Catalog  string         `json:"catalog_version"`
	Projects []ProjectCost  `json:"projects"`
	Lines    []EstimateLine `json:"lines"`
	Gross    int64          `json:"gross"`
	FreeTier FreeTierUsed   `json:"free_tier"`
	Total    int64          `json:"total"`
	// Projected : total attendu en fin de mois au rythme de la dernière heure
	// relevée (mois en cours seulement).
	Projected   int64     `json:"projected,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
}

// ParseMonth : "2026-11" → premier instant du mois, en UTC.
func ParseMonth(s string, now time.Time) (time.Time, error) {
	if s == "" {
		n := now.UTC()
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	}
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, ErrInvalidMonth
	}
	return t.UTC(), nil
}

// BuildEstimate : estimation à partir des totaux d'un mois. names : nom des
// projets. lastHour : coût de la dernière heure relevée par ressource horaire
// (pour la projection ; nil hors du mois en cours). remainingHours : heures
// restantes dans le mois.
func (c Catalog) BuildEstimate(month time.Time, totals []Total, names map[uuid.UUID]string,
	lastHour map[uuid.UUID]map[Meter]Milli, remainingHours int64, now time.Time) Estimate {
	e := Estimate{Month: month.Format("2006-01"), Currency: c.Currency, Catalog: c.Version,
		Projects: []ProjectCost{}, Lines: []EstimateLine{}, GeneratedAt: now.UTC()}

	byProject := map[uuid.UUID]Milli{}
	var gross, compute, database, buildMinutes Milli
	for _, t := range totals {
		cost := t.Cost()
		gross += cost
		byProject[t.ProjectID] += cost
		switch t.Meter {
		case MeterCompute:
			compute += cost
		case MeterDatabase, MeterDatabaseDisk:
			database += cost
		case MeterBuildMinutes:
			buildMinutes += cost
		}
		e.Lines = append(e.Lines, EstimateLine{ProjectID: t.ProjectID.String(), Meter: t.Meter,
			ResourceID: t.ResourceID.String(), ResourceName: t.ResourceName, Size: t.Size,
			Quantity: t.Quantity, Hours: t.Hours, Amount: cost.Units()})
	}

	free := c.freeTier(compute, database, buildMinutes)
	e.Gross, e.FreeTier = gross.Units(), free.used()
	e.Total = (gross - free.total).Units()

	for id, amount := range byProject {
		if id == uuid.Nil {
			continue // abonnement : il appartient à l'organisation, pas à un projet
		}
		e.Projects = append(e.Projects, ProjectCost{ProjectID: id.String(), Name: names[id], Amount: amount.Units()})
	}
	sort.Slice(e.Projects, func(i, j int) bool { return e.Projects[i].Amount > e.Projects[j].Amount })
	sort.SliceStable(e.Lines, func(i, j int) bool { return e.Lines[i].Amount > e.Lines[j].Amount })

	if lastHour != nil {
		e.Projected = c.project(totals, lastHour, remainingHours).Units()
	}
	return e
}

// NetCost : coût de la période (plafonds compris), offre gratuite déduite.
// C'est ce que la facturation réelle débite.
func (c Catalog) NetCost(totals []Total) Milli {
	var gross, compute, database, build Milli
	for _, t := range totals {
		cost := t.Cost()
		gross += cost
		switch t.Meter {
		case MeterCompute:
			compute += cost
		case MeterDatabase, MeterDatabaseDisk:
			database += cost
		case MeterBuildMinutes:
			build += cost
		}
	}
	return gross - c.freeTier(compute, database, build).total
}

type freeUse struct{ compute, database, build, total Milli }

func (f freeUse) used() FreeTierUsed {
	return FreeTierUsed{Compute: f.compute.Units(), Database: f.database.Units(), BuildMinutes: f.build.Units(), Total: f.total.Units()}
}

func (c Catalog) freeTier(compute, database, build Milli) freeUse {
	f := freeUse{
		compute:  minMilli(compute, c.Free.Compute),
		database: minMilli(database, c.Free.Database),
		build:    minMilli(build, c.BuildMinute*Milli(c.Free.BuildMinutes)),
	}
	f.total = f.compute + f.database + f.build
	return f
}

// project : chaque ressource horaire continue au coût de sa dernière heure
// jusqu'à la fin du mois (dans la limite de son plafond) ; les builds restent
// au niveau atteint. L'offre gratuite est déduite comme en fin de mois.
func (c Catalog) project(totals []Total, lastHour map[uuid.UUID]map[Meter]Milli, remaining int64) Milli {
	var compute, database, build, gross Milli
	for _, t := range totals {
		cost := t.Cost()
		if t.Meter.Hourly() {
			if rate, ok := lastHour[t.ResourceID][t.Meter]; ok {
				cost = minMilli(t.Sum+rate*Milli(remaining), t.Peak*HoursPerMonth)
			}
		}
		gross += cost
		switch t.Meter {
		case MeterCompute:
			compute += cost
		case MeterDatabase, MeterDatabaseDisk:
			database += cost
		case MeterBuildMinutes:
			build += cost
		}
	}
	return gross - c.freeTier(compute, database, build).total
}

// HoursLeft : heures restantes du mois après l'heure en cours.
func HoursLeft(month, now time.Time) int64 {
	end := month.AddDate(0, 1, 0)
	cur := now.UTC().Truncate(time.Hour)
	if !cur.Before(end) || cur.Before(month) {
		return 0
	}
	return int64(end.Sub(cur)/time.Hour) - 1
}

// CatalogView : grille publiée (formulaires de la console : « ≈ 3 000 FCFA/mois »).
type CatalogView struct {
	Version         string     `json:"version"`
	Currency        string     `json:"currency"`
	Sizes           []SizeView `json:"sizes"`
	DatabaseMonthly int64      `json:"database_monthly"`
	DiskGiMonthly   int64      `json:"disk_gi_monthly"`
	BuildMinute     int64      `json:"build_minute"`
	SiteDomain      int64      `json:"site_domain_monthly"`
	Free            FreeView   `json:"free_tier"`
	TopupMin        int64      `json:"topup_min"`
	TopupMax        int64      `json:"topup_max"`
	TrialCredit     int64      `json:"trial_credit"`
	TrialDays       int        `json:"trial_days"`
	GraceDays       int        `json:"grace_days"`
	// Postpayé : valeurs par défaut.
	PostpaidCreditLimit  int64 `json:"postpaid_credit_limit"`
	PostpaidTermsDays    int   `json:"postpaid_terms_days"`
	PostpaidReminderDays int   `json:"postpaid_reminder_days"`
	PostpaidSuspendDays  int   `json:"postpaid_suspend_days"`
	PurgeDays            int   `json:"purge_days"`
	DomainMarginBP       int   `json:"domain_margin_bp"`
	DomainRoundTo        int64 `json:"domain_round_to"`
	EURRateMilli         int64 `json:"eur_rate_milli"`
}

type SizeView struct {
	ID       string `json:"id"`
	CPUMilli int64  `json:"cpu_milli"`
	MemMi    int64  `json:"memory_mi"`
	Monthly  int64  `json:"monthly"`
}

type FreeView struct {
	Compute      int64 `json:"compute"`
	Database     int64 `json:"database"`
	BuildMinutes int64 `json:"build_minutes"`
}

// View : grille en unités.
func (c Catalog) View() CatalogView {
	v := CatalogView{Version: c.Version, Currency: c.Currency, DatabaseMonthly: c.DatabaseMonthly.Units(),
		DiskGiMonthly: c.DiskGiMonthly.Units(), BuildMinute: c.BuildMinute.Units(), SiteDomain: c.SiteDomain.Units(),
		Free:     FreeView{Compute: c.Free.Compute.Units(), Database: c.Free.Database.Units(), BuildMinutes: c.Free.BuildMinutes},
		TopupMin: c.TopupMin.Units(), TopupMax: c.TopupMax.Units(), TrialCredit: c.TrialCredit.Units(), TrialDays: c.TrialDays, GraceDays: c.GraceDays,
		PostpaidCreditLimit: c.PostpaidCreditLimit.Units(), PostpaidTermsDays: c.PostpaidTermsDays,
		PostpaidReminderDays: c.PostpaidReminderDays, PostpaidSuspendDays: c.PostpaidSuspendDays, PurgeDays: c.PurgeDays,
		DomainMarginBP: c.DomainMarginBP, DomainRoundTo: c.DomainRoundTo, EURRateMilli: int64(c.EURRate)}
	for _, s := range c.Sizes {
		v.Sizes = append(v.Sizes, SizeView{ID: s.ID, CPUMilli: s.CPUMilli, MemMi: s.MemMi, Monthly: s.Monthly.Units()})
	}
	return v
}

// Request DTOs.

// OrgPath : organisation de l'URL.
type OrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

// EstimateRequest : mois estimé (défaut : le mois en cours).
type EstimateRequest struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	Month          string `query:"month" description:"Mois au format AAAA-MM (défaut : le mois en cours)"`
}

// CatalogRequest : monnaie de la grille.
type CatalogRequest struct {
	Currency string `query:"currency" description:"Monnaie (XAF ou XOF ; défaut XAF)"`
}
