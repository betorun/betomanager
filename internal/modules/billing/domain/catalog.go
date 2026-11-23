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
	MeterEgressIP     Meter = "egress_ip"     // IP de sortie dédiée d'un projet
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
	// EgressIP : prix mensuel d'une IP de sortie dédiée.
	EgressIP Milli
	Free     FreeTier
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
	// PurgeDays : jours de suspension après lesquels les données de
	// l'organisation (conteneurs et disques, bases, sites) sont supprimées,
	// après une copie finale des bases (0 : jamais).
	PurgeDays int
	// Domaines revendus : prix = prix du registraire (en euros) × EURRate,
	// plus la marge (points de base), arrondi au palier supérieur.
	DomainMarginBP int
	DomainRoundTo  int64
	// EURRate : unités de la monnaie pour 1 €, en millièmes (655957 :
	// 655,957 FCFA, parité fixe).
	EURRate Milli
}

// DomainPrice : prix de vente d'un domaine (unités de la monnaie) à partir
// du prix du registraire en centimes d'euro ; 0 si la grille ne le permet pas.
func (c Catalog) DomainPrice(eurCents int64) int64 {
	if c.EURRate <= 0 || eurCents <= 0 {
		return 0
	}
	// millièmes d'unité = centimes × taux (millièmes) / 100
	base := eurCents * int64(c.EURRate) / 100
	withMargin := base * int64(10000+c.DomainMarginBP) / 10000
	units := ceilDiv(withMargin, int64(MilliPerUnit))
	if step := c.DomainRoundTo; step > 1 {
		units = ceilDiv(units, step) * step
	}
	return units
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
