package service

import (
	"context"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/status/domain"
)

// Signals : ce que les sondes lisent en base (tables des autres modules,
// lues en SQL : pas de dépendance à leur code).
type Signals interface {
	// VPSSignals : réconciliation (compute_reconcile_runs) et demandes (compute_requests).
	VPSSignals(ctx context.Context) (domain.VPSSignals, error)
	// OldestDomainOrdering : plus ancienne commande de domaine payée et pas
	// encore passée au registraire (nil : aucune).
	OldestDomainOrdering(ctx context.Context) (*time.Time, error)
}

// VPSProbe : réconciliation récente, demandes qui avancent.
func VPSProbe(sig Signals) Probe {
	return ProbeFunc(func(ctx context.Context, now time.Time) (domain.Check, error) {
		s, err := sig.VPSSignals(ctx)
		if err != nil {
			return domain.Check{}, err
		}
		return domain.VPSCheck(s, now), nil
	})
}

// DomainsProbe : commandes de domaines qui partent chez le registraire.
func DomainsProbe(sig Signals) Probe {
	return ProbeFunc(func(ctx context.Context, now time.Time) (domain.Check, error) {
		oldest, err := sig.OldestDomainOrdering(ctx)
		if err != nil {
			return domain.Check{}, err
		}
		return domain.DomainsCheck(oldest, now), nil
	})
}

// Features : services en marche sur cette plateforme ; un service arrêté
// garde son composant, en état unknown.
type Features struct {
	VPS     bool
	Domains bool
}

// DefaultComponents : registre des composants affichés, dans l'ordre. Pour
// ajouter un composant : sa clé et son nom (domain), son entrée au
// catalogue status.json, sa sonde ici.
func DefaultComponents(sig Signals, f Features) []Component {
	vps, domains := Probe(nil), Probe(nil)
	if sig != nil && f.VPS {
		vps = VPSProbe(sig)
	}
	if sig != nil && f.Domains {
		domains = DomainsProbe(sig)
	}
	return []Component{
		{Key: domain.ComponentAPI, Name: domain.NameAPI, Probe: Operational},
		{Key: domain.ComponentVPS, Name: domain.NameVPS, Probe: vps},
		{Key: domain.ComponentDomains, Name: domain.NameDomains, Probe: domains},
	}
}

func trim(s string) string { return strings.TrimSpace(s) }
