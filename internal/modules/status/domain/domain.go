// Package domain : page de statut publique (docs/modules/status.md). L'état
// de chaque composant est calculé par une sonde qui ne lit que la base ; un
// incident ouvert par l'équipe l'aggrave si son impact est pire.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Composants affichés.
const (
	ComponentAPI     = "api"
	ComponentVPS     = "vps"
	ComponentDomains = "domains"
)

// Noms des composants (traduits par le catalogue, core/i18n/catalog/status.json).
const (
	NameAPI     = "Console et API"
	NameVPS     = "Serveurs VPS : commande et gestion"
	NameDomains = "Noms de domaine"
)

// États d'un composant, du meilleur au pire (unknown à part).
const (
	StateOperational = "operational"
	StateDegraded    = "degraded"
	StateOutage      = "outage"
	StateUnknown     = "unknown"
)

// rank : gravité ; unknown ne compte pas comme une panne.
func rank(state string) int {
	switch state {
	case StateOperational:
		return 1
	case StateDegraded:
		return 2
	case StateOutage:
		return 3
	}
	return 0
}

// Worse : l'état le plus grave des deux (unknown perd toujours).
func Worse(a, b string) string {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Overall : état d'ensemble : le pire des composants connus ; unknown si
// aucun n'est connu.
func Overall(states []string) string {
	out := StateUnknown
	for _, s := range states {
		out = Worse(out, s)
	}
	return out
}

// Impact d'un incident.
const (
	ImpactMinor       = "minor"
	ImpactMajor       = "major"
	ImpactMaintenance = "maintenance"
)

// Statut d'un incident.
const (
	StatusInvestigating = "investigating"
	StatusIdentified    = "identified"
	StatusMonitoring    = "monitoring"
	StatusResolved      = "resolved"
)

var (
	ErrIncidentNotFound    = errors.New("incident not found")
	ErrIncidentResolved    = errors.New("this incident is resolved")
	ErrInvalidIncident     = errors.New("title and message are required in French and in English")
	ErrIncidentTooLong     = errors.New("incident title or message is too long")
	ErrInvalidImpact       = errors.New("impact must be minor, major or maintenance")
	ErrInvalidStatus       = errors.New("status must be investigating, identified, monitoring or resolved")
	ErrInvalidComponents   = errors.New("at least one known component is required")
	ErrInvalidWindow       = errors.New("a maintenance needs a start and an end after it")
	ErrInvalidStatusUpdate = errors.New("message is required in French and in English")
)

// Limites des textes.
const (
	MaxTitle   = 200
	MaxMessage = 5000
)

// Incident : panne, dégradation ou maintenance annoncée par l'équipe.
type Incident struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	TitleFR    string    `json:"title_fr"`
	TitleEN    string    `json:"title_en"`
	MessageFR  string    `json:"message_fr"`
	MessageEN  string    `json:"message_en"`
	Components []string  `gorm:"serializer:json;type:jsonb;not null" json:"components"`
	Impact     string    `json:"impact"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	// ResolvedAt : posé quand le statut passe à resolved.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// StartsAt / EndsAt : fenêtre d'une maintenance programmée.
	StartsAt  *time.Time       `json:"starts_at,omitempty"`
	EndsAt    *time.Time       `json:"ends_at,omitempty"`
	CreatedBy string           `json:"created_by"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedBy string           `json:"updated_by"`
	UpdatedAt time.Time        `json:"updated_at"`
	Updates   []IncidentUpdate `gorm:"foreignKey:IncidentID" json:"updates"`
}

func (Incident) TableName() string { return "status_incidents" }

// IncidentUpdate : message daté ajouté au fil de l'incident.
type IncidentUpdate struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	IncidentID uuid.UUID `gorm:"type:uuid" json:"-"`
	Status     string    `json:"status"`
	MessageFR  string    `json:"message_fr"`
	MessageEN  string    `json:"message_en"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

func (IncidentUpdate) TableName() string { return "status_incident_updates" }

// Resolved : incident clos.
func (i *Incident) Resolved() bool { return i.Status == StatusResolved }

// Upcoming : maintenance programmée pas encore commencée.
func (i *Incident) Upcoming(now time.Time) bool {
	return i.Impact == ImpactMaintenance && !i.Resolved() && i.StartsAt != nil && now.Before(*i.StartsAt)
}

// Active : incident ouvert qui pèse sur l'état des composants (une
// maintenance seulement pendant sa fenêtre).
func (i *Incident) Active(now time.Time) bool {
	if i.Resolved() {
		return false
	}
	if i.Impact == ImpactMaintenance {
		return i.StartsAt != nil && !now.Before(*i.StartsAt) && (i.EndsAt == nil || now.Before(*i.EndsAt))
	}
	return true
}

// ImpactState : état imposé aux composants touchés.
func ImpactState(impact string) string {
	switch impact {
	case ImpactMajor:
		return StateOutage
	case ImpactMinor, ImpactMaintenance:
		return StateDegraded
	}
	return StateOperational
}

// ValidImpact / ValidStatus : valeurs connues.
func ValidImpact(s string) bool {
	return s == ImpactMinor || s == ImpactMajor || s == ImpactMaintenance
}

func ValidStatus(s string) bool {
	return s == StatusInvestigating || s == StatusIdentified || s == StatusMonitoring || s == StatusResolved
}

// IncidentInput : ce que l'équipe rédige (création, modification).
type IncidentInput struct {
	TitleFR    string     `json:"title_fr"`
	TitleEN    string     `json:"title_en"`
	MessageFR  string     `json:"message_fr"`
	MessageEN  string     `json:"message_en"`
	Components []string   `json:"components"`
	Impact     string     `json:"impact"`
	Status     string     `json:"status,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
}

// Normalize : espaces retirés, composants dédoublonnés.
func (in IncidentInput) Normalize() IncidentInput {
	out := in
	out.TitleFR, out.TitleEN = strings.TrimSpace(in.TitleFR), strings.TrimSpace(in.TitleEN)
	out.MessageFR, out.MessageEN = strings.TrimSpace(in.MessageFR), strings.TrimSpace(in.MessageEN)
	out.Impact, out.Status = strings.TrimSpace(in.Impact), strings.TrimSpace(in.Status)
	seen := map[string]bool{}
	out.Components = nil
	for _, c := range in.Components {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			out.Components = append(out.Components, c)
		}
	}
	return out
}

// Validate : textes dans les deux langues, composants connus, fenêtre de maintenance.
func (in IncidentInput) Validate(known func(string) bool) error {
	if in.TitleFR == "" || in.TitleEN == "" || in.MessageFR == "" || in.MessageEN == "" {
		return ErrInvalidIncident
	}
	if len(in.TitleFR) > MaxTitle || len(in.TitleEN) > MaxTitle || len(in.MessageFR) > MaxMessage || len(in.MessageEN) > MaxMessage {
		return ErrIncidentTooLong
	}
	if !ValidImpact(in.Impact) {
		return ErrInvalidImpact
	}
	if in.Status != "" && !ValidStatus(in.Status) {
		return ErrInvalidStatus
	}
	if len(in.Components) == 0 {
		return ErrInvalidComponents
	}
	for _, c := range in.Components {
		if !known(c) {
			return ErrInvalidComponents
		}
	}
	if in.Impact == ImpactMaintenance {
		if in.StartsAt == nil || in.EndsAt == nil || !in.EndsAt.After(*in.StartsAt) {
			return ErrInvalidWindow
		}
	} else if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return ErrInvalidWindow
	}
	return nil
}

// UpdateInput : message ajouté au fil d'un incident, avec son nouveau statut.
type UpdateInput struct {
	Status    string `json:"status"`
	MessageFR string `json:"message_fr"`
	MessageEN string `json:"message_en"`
}

func (in UpdateInput) Validate() error {
	if !ValidStatus(strings.TrimSpace(in.Status)) {
		return ErrInvalidStatus
	}
	fr, en := strings.TrimSpace(in.MessageFR), strings.TrimSpace(in.MessageEN)
	if fr == "" || en == "" {
		return ErrInvalidStatusUpdate
	}
	if len(fr) > MaxMessage || len(en) > MaxMessage {
		return ErrIncidentTooLong
	}
	return nil
}

// ── Sondes ──────────────────────────────────────────────────────────────

// Check : résultat d'une sonde. Reason : code court pour l'équipe, jamais
// affiché sur la page publique.
type Check struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// Seuils des sondes VPS et domaines.
const (
	// La réconciliation tourne toutes les 10 minutes : trois passes manquées.
	ReconcileMaxAge = 30 * time.Minute
	// Une demande reste « pending » tant que le relais ne l'a pas lancée.
	PendingMaxAge = 15 * time.Minute
	// Une demande lancée finit en quelques minutes ; 6 h : bloquée.
	StartedMaxAge = 6 * time.Hour
	// Une commande de domaine payée part chez le registraire en quelques minutes.
	DomainOrderingMaxAge = time.Hour
)

// Raisons (pour l'équipe).
const (
	ReasonReconcileFailed = "reconcile_failed"
	ReasonReconcileStale  = "reconcile_stale"
	ReasonNoReconcile     = "reconcile_never_ran"
	ReasonRequestsPending = "requests_pending_stuck"
	ReasonRequestsStarted = "requests_started_stuck"
	ReasonDomainsOrdering = "domain_orders_stuck"
	ReasonProbeFailed     = "probe_failed"
	ReasonDisabled        = "disabled"
)

// VPSSignals : ce que la sonde VPS lit en base.
type VPSSignals struct {
	// LastFinished : dernière passe de réconciliation terminée (nil : aucune).
	LastFinished *time.Time
	// LastFailed : cette passe a fini en erreur.
	LastFailed bool
	// FirstStarted : première passe jamais lancée (nil : aucune).
	FirstStarted *time.Time
	// OldestPending / OldestStarted : plus ancienne demande en attente / lancée.
	OldestPending *time.Time
	OldestStarted *time.Time
}

// VPSCheck : réconciliation récente et sans erreur, et aucune demande bloquée.
func VPSCheck(s VPSSignals, now time.Time) Check {
	c := Check{State: StateOperational}
	switch {
	case s.LastFinished == nil && s.FirstStarted == nil:
		c = Check{State: StateUnknown, Reason: ReasonNoReconcile}
	case s.LastFinished == nil:
		if now.Sub(*s.FirstStarted) > ReconcileMaxAge {
			c = Check{State: StateDegraded, Reason: ReasonReconcileStale}
		}
	case s.LastFailed:
		c = Check{State: StateDegraded, Reason: ReasonReconcileFailed}
	case now.Sub(*s.LastFinished) > ReconcileMaxAge:
		c = Check{State: StateDegraded, Reason: ReasonReconcileStale}
	}
	// Une demande bloquée dégrade même sans réconciliation (fournisseur sans clés).
	switch {
	case s.OldestPending != nil && now.Sub(*s.OldestPending) > PendingMaxAge:
		c = Check{State: Worse(c.State, StateDegraded), Reason: ReasonRequestsPending}
	case s.OldestStarted != nil && now.Sub(*s.OldestStarted) > StartedMaxAge:
		c = Check{State: Worse(c.State, StateDegraded), Reason: ReasonRequestsStarted}
	}
	return c
}

// DomainsCheck : aucune commande payée restée sans suite plus d'une heure.
func DomainsCheck(oldestOrdering *time.Time, now time.Time) Check {
	if oldestOrdering != nil && now.Sub(*oldestOrdering) > DomainOrderingMaxAge {
		return Check{State: StateDegraded, Reason: ReasonDomainsOrdering}
	}
	return Check{State: StateOperational}
}
