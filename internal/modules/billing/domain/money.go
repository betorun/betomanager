// Package domain : règles de la facturation (docs/adr/0012-facturation.md).
// Aucune dépendance vers la base, le cluster ou HTTP : tout se teste seul.
package domain

// Milli : montant en millièmes d'unité de la monnaie (1 FCFA = 1000).
// Toujours un entier : jamais de nombre à virgule pour de l'argent.
type Milli int64

// MilliPerUnit : millièmes dans une unité.
const MilliPerUnit Milli = 1000

// Units : montant arrondi à l'unité (au plus proche, la moitié vers le haut),
// pour l'affichage et la facture.
func (m Milli) Units() int64 {
	if m < 0 {
		return -int64((-m + MilliPerUnit/2) / MilliPerUnit)
	}
	return int64((m + MilliPerUnit/2) / MilliPerUnit)
}

// FromUnits : montant entier en unités.
func FromUnits(units int64) Milli { return Milli(units) * MilliPerUnit }

func minMilli(a, b Milli) Milli {
	if a < b {
		return a
	}
	return b
}
