package domain

// HoursPerMonth : un mois facturé ne dépasse jamais 730 heures.
const HoursPerMonth = 730

// Meter : ce qui est compté.
type Meter string

const (
	MeterCompute      Meter = "compute"       // conteneur : taille × réplicas
	MeterDisk         Meter = "disk"          // disque d'un conteneur, en Go
	MeterDatabase     Meter = "database"      // instance d'une base
	MeterDatabaseDisk Meter = "database_disk" // disque d'une base, en Go
	MeterBuildMinutes Meter = "build_minutes" // minutes de build
	MeterSiteDomain   Meter = "site_domain"   // domaine personnalisé d'un site
)

// Hourly : le compteur est-il facturé à l'heure (plafonné au mois) ? Les
// minutes de build se paient à l'unité.
func (m Meter) Hourly() bool { return m != MeterBuildMinutes }

// Size : taille de conteneur. Un conteneur est classé dans la plus petite
// taille qui couvre ses limites, réplica par réplica.
type Size struct {
	ID       string // mini, small, medium, large, xlarge ; custom au-delà
	CPUMilli int64
	MemMi    int64
	Monthly  Milli
}

// SizeCustom : au-delà de la plus grande taille, prix proportionnel.
const SizeCustom = "custom"

// FreeTier : offre gratuite par organisation et par mois.
type FreeTier struct {
	// Compute : montant de calcul offert (par exemple, un conteneur Mini).
	Compute Milli
	// Database : montant de base offert (par exemple, une base de 1 Go).
	Database Milli
	// BuildMinutes : minutes de build offertes.
	BuildMinutes int64
}

// Catalog : grille de prix d'une monnaie. Aucun montant n'est fixé dans le
// code : la grille vit en base (billing_catalogs), modifiable par l'équipe de
// la plateforme. Chaque modification crée une version ; chaque ligne d'usage
// garde la version et le prix appliqués, le passé n'est jamais réécrit.
type Catalog struct {
	Version  string
	Currency string
	// Sizes : de la plus petite à la plus grande.
	Sizes           []Size
	DatabaseMonthly Milli
	DiskGiMonthly   Milli
	BuildMinute     Milli
	SiteDomain      Milli
	Free            FreeTier
	// TopupMin / TopupMax : montant d'une recharge (0 : recharges fermées).
	TopupMin, TopupMax Milli
	// TrialCredit : crédit d'essai offert à l'activation de la facturation
	// réelle, valable TrialDays jours (0 : pas de crédit d'essai).
	TrialCredit Milli
	TrialDays   int
	// GraceDays : jours de solde négatif tolérés avant la suspension douce
	// (plafonnés aussi à GraceDays jours de consommation de dette).
	GraceDays int
	// Postpayé (valeurs par défaut, réglables client par client) : plafond de
	// dette, échéance des factures, relance et suspension après l'échéance.
	PostpaidCreditLimit  Milli
	PostpaidTermsDays    int
	PostpaidReminderDays int
	PostpaidSuspendDays  int
}

// DefaultCurrency : monnaie d'un compte sans réglage.
const DefaultCurrency = "XAF"

// SizeFor : taille d'un réplica et son prix mensuel. Au-delà de la plus grande
// taille : proportionnel à elle, sur la ressource la plus demandée (au
// millième près, arrondi au-dessus).
func (c Catalog) SizeFor(cpuMilli, memMi int64) (string, Milli) {
	for _, s := range c.Sizes {
		if cpuMilli <= s.CPUMilli && memMi <= s.MemMi {
			return s.ID, s.Monthly
		}
	}
	top := c.Sizes[len(c.Sizes)-1]
	ratio := max(ceilDiv(cpuMilli*1000, top.CPUMilli), ceilDiv(memMi*1000, top.MemMi))
	return SizeCustom, Milli(ceilDiv(int64(top.Monthly)*ratio, 1000))
}

// PerHour : prix horaire d'un prix mensuel (arrondi au millième supérieur :
// 730 heures ne coûtent jamais moins que le mois, le plafond rattrape
// l'excédent).
func PerHour(monthly Milli) Milli { return Milli(ceilDiv(int64(monthly), HoursPerMonth)) }

func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
