package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidCatalog : grille refusée (le message dit pourquoi).
var ErrInvalidCatalog = errors.New("invalid price catalog")

// CatalogError : grille refusée, avec la raison.
type CatalogError struct{ Reason string }

func (e *CatalogError) Error() string { return "invalid price catalog: " + e.Reason }
func (e *CatalogError) Unwrap() error { return ErrInvalidCatalog }

func invalid(format string, args ...any) error {
	return &CatalogError{Reason: fmt.Sprintf(format, args...)}
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	sizeIDRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
)

// maxSizes : tailles proposées au plus.
const maxSizes = 12

// NormalizeCurrency : code ISO 4217 en majuscules, ou "".
func NormalizeCurrency(raw string) string {
	c := strings.ToUpper(strings.TrimSpace(raw))
	if !currencyRe.MatchString(c) {
		return ""
	}
	return c
}

// CatalogUpdate : grille saisie par l'équipe (montants en unités de la
// monnaie, par mois ; une minute pour les builds).
type CatalogUpdate struct {
	Currency             string     `path:"currency" json:"-" description:"Monnaie (code ISO 4217, ex. XAF)"`
	Sizes                []SizeView `json:"sizes" description:"Tailles de conteneur, de la plus petite à la plus grande"`
	DatabaseMonthly      int64      `json:"database_monthly" description:"Prix mensuel d'une instance de base"`
	DiskGiMonthly        int64      `json:"disk_gi_monthly" description:"Prix mensuel d'un Go de disque (base ou conteneur)"`
	BuildMinute          int64      `json:"build_minute" description:"Prix d'une minute de build au-delà de l'offre gratuite"`
	SiteDomain           int64      `json:"site_domain_monthly" description:"Prix mensuel d'un domaine personnalisé de site (0 : gratuit)"`
	EgressIP             int64      `json:"egress_ip_monthly" description:"Prix mensuel d'une IP de sortie dédiée d'un projet (0 : gratuite)"`
	StorageGBMonthly     int64      `json:"storage_gb_monthly" description:"Prix mensuel d'un Go de stockage objet (buckets ; 0 : gratuit)"`
	Free                 FreeView   `json:"free_tier" description:"Offre gratuite par organisation et par mois"`
	TopupMin             int64      `json:"topup_min" description:"Recharge minimale (0 avec topup_max : recharges fermées)"`
	TopupMax             int64      `json:"topup_max" description:"Recharge maximale"`
	TrialCredit          int64      `json:"trial_credit" description:"Crédit d'essai offert à l'activation de la facturation (0 : aucun)"`
	TrialDays            int        `json:"trial_days" description:"Validité du crédit d'essai, en jours"`
	GraceDays            int        `json:"grace_days" description:"Jours de solde négatif tolérés avant l'arrêt des conteneurs"`
	PostpaidCreditLimit  int64      `json:"postpaid_credit_limit" description:"Postpayé : plafond de dette par défaut"`
	PostpaidTermsDays    int        `json:"postpaid_terms_days" description:"Postpayé : échéance des factures, en jours"`
	PostpaidReminderDays int        `json:"postpaid_reminder_days" description:"Postpayé : relance, en jours après l'échéance"`
	PostpaidSuspendDays  int        `json:"postpaid_suspend_days" description:"Postpayé : suspension, en jours après l'échéance"`
	PurgeDays            int        `json:"purge_days" description:"Jours de suspension avant la suppression des données (0 : jamais)"`
	DomainMarginBP       int        `json:"domain_margin_bp" description:"Domaines : marge sur le prix du registraire, en points de base (2000 = 20 %)"`
	DomainRoundTo        int64      `json:"domain_round_to" description:"Domaines : prix arrondi au palier supérieur (ex. 500)"`
	EURRateMilli         int64      `json:"eur_rate_milli" description:"Domaines : unités de la monnaie pour 1 €, en millièmes (655957 pour le FCFA)"`
}

// CatalogFromUpdate : grille validée. Les tailles doivent croître (CPU,
// mémoire et prix) : un conteneur est classé dans la plus petite qui le couvre.
func CatalogFromUpdate(u CatalogUpdate) (Catalog, error) {
	currency := NormalizeCurrency(u.Currency)
	if currency == "" {
		return Catalog{}, invalid("currency must be an ISO 4217 code such as XAF")
	}
	if len(u.Sizes) == 0 || len(u.Sizes) > maxSizes {
		return Catalog{}, invalid("between 1 and %d container sizes", maxSizes)
	}
	c := Catalog{Currency: currency}
	seen := map[string]bool{}
	for i, s := range u.Sizes {
		id := strings.TrimSpace(s.ID)
		switch {
		case !sizeIDRe.MatchString(id) || id == SizeCustom:
			return Catalog{}, invalid("size %d: id must be lowercase letters, digits or dashes (not %q)", i+1, SizeCustom)
		case seen[id]:
			return Catalog{}, invalid("size %q appears twice", id)
		case s.CPUMilli <= 0 || s.MemMi <= 0 || s.Monthly <= 0:
			return Catalog{}, invalid("size %q: CPU, memory and price must be positive", id)
		}
		if i > 0 {
			prev := u.Sizes[i-1]
			if s.CPUMilli < prev.CPUMilli || s.MemMi < prev.MemMi || s.Monthly < prev.Monthly ||
				(s.CPUMilli == prev.CPUMilli && s.MemMi == prev.MemMi) {
				return Catalog{}, invalid("size %q must be larger than %q (CPU, memory and price)", id, prev.ID)
			}
		}
		seen[id] = true
		c.Sizes = append(c.Sizes, Size{ID: id, CPUMilli: s.CPUMilli, MemMi: s.MemMi, Monthly: FromUnits(s.Monthly)})
	}
	for name, v := range map[string]int64{
		"database_monthly": u.DatabaseMonthly, "disk_gi_monthly": u.DiskGiMonthly, "build_minute": u.BuildMinute,
		"site_domain_monthly": u.SiteDomain, "egress_ip_monthly": u.EgressIP, "storage_gb_monthly": u.StorageGBMonthly, "free_tier.compute": u.Free.Compute, "free_tier.database": u.Free.Database,
		"free_tier.build_minutes": u.Free.BuildMinutes, "topup_min": u.TopupMin, "topup_max": u.TopupMax,
		"trial_credit": u.TrialCredit, "trial_days": int64(u.TrialDays), "grace_days": int64(u.GraceDays),
		"postpaid_credit_limit": u.PostpaidCreditLimit, "postpaid_terms_days": int64(u.PostpaidTermsDays),
		"postpaid_reminder_days": int64(u.PostpaidReminderDays), "postpaid_suspend_days": int64(u.PostpaidSuspendDays),
		"purge_days":       int64(u.PurgeDays),
		"domain_margin_bp": int64(u.DomainMarginBP), "domain_round_to": u.DomainRoundTo, "eur_rate_milli": u.EURRateMilli,
	} {
		if v < 0 {
			return Catalog{}, invalid("%s cannot be negative", name)
		}
	}
	c.DatabaseMonthly, c.DiskGiMonthly = FromUnits(u.DatabaseMonthly), FromUnits(u.DiskGiMonthly)
	c.BuildMinute, c.SiteDomain = FromUnits(u.BuildMinute), FromUnits(u.SiteDomain)
	c.EgressIP, c.StorageGBMonthly = FromUnits(u.EgressIP), FromUnits(u.StorageGBMonthly)
	c.Free = FreeTier{Compute: FromUnits(u.Free.Compute), Database: FromUnits(u.Free.Database), BuildMinutes: u.Free.BuildMinutes}
	if u.TopupMax > 0 && (u.TopupMin <= 0 || u.TopupMin > u.TopupMax) {
		return Catalog{}, invalid("topup_min must be positive and at most topup_max")
	}
	if u.TrialCredit > 0 && u.TrialDays <= 0 {
		return Catalog{}, invalid("trial_days must be positive when a trial credit is offered")
	}
	c.TopupMin, c.TopupMax = FromUnits(u.TopupMin), FromUnits(u.TopupMax)
	c.TrialCredit, c.TrialDays = FromUnits(u.TrialCredit), u.TrialDays
	if u.GraceDays > 30 {
		return Catalog{}, invalid("grace_days must be at most 30")
	}
	c.GraceDays = u.GraceDays
	if u.PostpaidSuspendDays > 0 && u.PostpaidSuspendDays < u.PostpaidReminderDays {
		return Catalog{}, invalid("postpaid_suspend_days must come after postpaid_reminder_days")
	}
	if u.PurgeDays > 0 && u.PurgeDays < MinPurgeDays {
		return Catalog{}, invalid("purge_days must be 0 (never) or at least %d", MinPurgeDays)
	}
	c.PurgeDays = u.PurgeDays
	c.PostpaidCreditLimit = FromUnits(u.PostpaidCreditLimit)
	c.PostpaidTermsDays, c.PostpaidReminderDays, c.PostpaidSuspendDays = u.PostpaidTermsDays, u.PostpaidReminderDays, u.PostpaidSuspendDays
	c.DomainMarginBP, c.DomainRoundTo, c.EURRate = u.DomainMarginBP, u.DomainRoundTo, Milli(u.EURRateMilli)
	return c, nil
}

// Update : la grille sous forme de saisie (stockage en base, en unités).
func (c Catalog) Update() CatalogUpdate {
	v := c.View()
	return CatalogUpdate{Currency: c.Currency, Sizes: v.Sizes, DatabaseMonthly: v.DatabaseMonthly,
		DiskGiMonthly: v.DiskGiMonthly, BuildMinute: v.BuildMinute, SiteDomain: v.SiteDomain, EgressIP: v.EgressIP, StorageGBMonthly: v.StorageGBMonthly, Free: v.Free,
		TopupMin: v.TopupMin, TopupMax: v.TopupMax, TrialCredit: v.TrialCredit, TrialDays: v.TrialDays, GraceDays: v.GraceDays,
		PostpaidCreditLimit: v.PostpaidCreditLimit, PostpaidTermsDays: v.PostpaidTermsDays,
		PostpaidReminderDays: v.PostpaidReminderDays, PostpaidSuspendDays: v.PostpaidSuspendDays, PurgeDays: v.PurgeDays,
		DomainMarginBP: v.DomainMarginBP, DomainRoundTo: v.DomainRoundTo, EURRateMilli: v.EURRateMilli}
}

// CatalogVersionView : une version de grille (administration).
type CatalogVersionView struct {
	CatalogView
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

// CatalogsResponse : grilles en vigueur, une par monnaie.
type CatalogsResponse struct {
	Catalogs []CatalogVersionView `json:"catalogs"`
}
