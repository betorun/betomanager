// Package certstatus : état des certificats HTTPS délivrés par cert-manager,
// tel que la console l'affiche : en cours, actif, en échec. Aucun texte de
// cert-manager n'est renvoyé : seulement un état et une raison, traduits par
// la console. Sans dépendance (la CLI l'embarque) ; la lecture du cluster est
// dans infrastructure/certread.
package certstatus

import "time"

// États d'un certificat.
const (
	StatePending = "pending"
	StateActive  = "active"
	StateFailed  = "failed"
)

// Raisons d'un échec.
const (
	ReasonRateLimited = "rate_limited"
	ReasonChallenge   = "challenge"
	ReasonUnknown     = "unknown"
)

// Status : état du certificat d'un domaine, tel que la console l'affiche.
type Status struct {
	State string `json:"state" description:"pending, active ou failed"`
	// Reason : seulement en échec (rate_limited, challenge, unknown).
	Reason string `json:"reason,omitempty" description:"Raison de l'échec : rate_limited, challenge ou unknown"`
	// NotAfter et RenewalAt : seulement un certificat actif.
	NotAfter  *time.Time `json:"not_after,omitempty" description:"Fin de validité du certificat"`
	RenewalAt *time.Time `json:"renewal_at,omitempty" description:"Renouvellement automatique prévu"`
}

// Pending : certificat demandé, pas encore délivré (ou pas encore créé).
func Pending() *Status { return &Status{State: StatePending} }
