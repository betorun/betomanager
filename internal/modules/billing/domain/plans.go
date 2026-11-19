package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Plans (ADR 0012, phase 5) : un plan fixe les limites de chaque projet de
// l'organisation et un abonnement mensuel. L'abonnement est mesuré comme une
// ressource (MeterPlan) : à l'heure, plafonné au mois, hors offre gratuite.

// MeterPlan : abonnement au plan de l'organisation.
const MeterPlan Meter = "plan"

// KindPlan : réservation « abonnement » d'une organisation.
const KindPlan ResourceKind = "plan"

// DefaultPlan : plan d'une organisation sans réglage.
const DefaultPlan = "free"

var (
	ErrPlanNotFound     = errors.New("plan not found")
	ErrInvalidPlan      = errors.New("invalid plan")
	ErrPlanNeedsBilling = errors.New("a paid plan needs billing to be active for this organization")
	ErrPlanBelowUsage   = errors.New("the plan's limits are below what some projects already reserve")
)

// PlanError : plan refusé, avec la raison.
type PlanError struct{ Reason string }

func (e *PlanError) Error() string { return "invalid plan: " + e.Reason }
func (e *PlanError) Unwrap() error { return ErrInvalidPlan }

// PlanBelowUsageError : projets qui réservent déjà plus que le plan.
type PlanBelowUsageError struct{ Projects []string }

func (e *PlanBelowUsageError) Error() string {
	return "the plan's limits are below what these projects already reserve: " + strings.Join(e.Projects, ", ")
}
func (e *PlanBelowUsageError) Unwrap() error { return ErrPlanBelowUsage }

// Plan : un plan.
type Plan struct {
	ID           string
	Name         string
	Position     int
	Public       bool
	CPULimit     string
	MemoryLimit  string
	StorageLimit string
	// Fees : abonnement mensuel par monnaie, en unités.
	Fees map[string]int64
}

// Fee : abonnement mensuel dans une monnaie (0 si non renseigné).
func (p Plan) Fee(currency string) Milli { return FromUnits(p.Fees[strings.ToUpper(currency)]) }

// Limits : limites en millicœurs et Mo (contrôle avant de changer de plan).
func (p Plan) Limits() (cpuMilli, memMi int64, err error) {
	cpu, err := resource.ParseQuantity(p.CPULimit)
	if err != nil {
		return 0, 0, ErrInvalidPlan
	}
	mem, err := resource.ParseQuantity(p.MemoryLimit)
	if err != nil {
		return 0, 0, ErrInvalidPlan
	}
	return cpu.MilliValue(), mem.Value() >> 20, nil
}

// PlanView : un plan vu par le client (abonnement dans sa monnaie).
type PlanView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CPULimit     string `json:"cpu_limit"`
	MemoryLimit  string `json:"memory_limit"`
	StorageLimit string `json:"storage_limit"`
	Monthly      int64  `json:"monthly"`
	Currency     string `json:"currency"`
	Public       bool   `json:"public"`
}

// View : vue dans une monnaie.
func (p Plan) View(currency string) PlanView {
	return PlanView{ID: p.ID, Name: p.Name, CPULimit: p.CPULimit, MemoryLimit: p.MemoryLimit, StorageLimit: p.StorageLimit,
		Monthly: p.Fee(currency).Units(), Currency: currency, Public: p.Public}
}

// PlansResponse : plans proposés.
type PlansResponse struct {
	Plans []PlanView `json:"plans"`
}

// OrgPlan : plan d'une organisation et plans proposés.
type OrgPlan struct {
	Current PlanView   `json:"current"`
	Since   *time.Time `json:"since,omitempty"`
	Plans   []PlanView `json:"plans"`
}

// AdminPlan : un plan, toutes monnaies (administration).
type AdminPlan struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Position     int              `json:"position"`
	Public       bool             `json:"public"`
	CPULimit     string           `json:"cpu_limit"`
	MemoryLimit  string           `json:"memory_limit"`
	StorageLimit string           `json:"storage_limit"`
	Fees         map[string]int64 `json:"fees"`
}

// AdminPlansResponse : tous les plans.
type AdminPlansResponse struct {
	Plans []AdminPlan `json:"plans"`
}

// PlanUpdate : créer ou modifier un plan (équipe). Les organisations déjà sur
// ce plan gardent leurs limites jusqu'à leur prochain changement de plan ;
// l'abonnement suit au relevé suivant.
type PlanUpdate struct {
	ID           string           `path:"planID" json:"-" description:"Identifiant du plan (lettres minuscules, chiffres, tirets)"`
	Name         string           `json:"name" binding:"required" description:"Nom affiché"`
	Position     int              `json:"position" description:"Ordre d'affichage"`
	Public       bool             `json:"public" description:"Proposé aux clients (sinon attribué par l'équipe seulement)"`
	CPULimit     string           `json:"cpu_limit" binding:"required" description:"Limite CPU par projet (ex. 4000m)"`
	MemoryLimit  string           `json:"memory_limit" binding:"required" description:"Limite mémoire par projet (ex. 8Gi)"`
	StorageLimit string           `json:"storage_limit" binding:"required" description:"Limite de stockage par projet (ex. 40Gi)"`
	Fees         map[string]int64 `json:"fees" description:"Abonnement mensuel par monnaie, en unités (ex. {\"XAF\": 5000})"`
}

var planIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,29}$`)

// PlanFromUpdate : plan validé.
func PlanFromUpdate(u PlanUpdate) (Plan, error) {
	bad := func(reason string) error { return &PlanError{Reason: reason} }
	id := strings.TrimSpace(u.ID)
	if !planIDRe.MatchString(id) {
		return Plan{}, bad("id must be lowercase letters, digits or dashes")
	}
	name := strings.TrimSpace(u.Name)
	if name == "" || len(name) > 60 {
		return Plan{}, bad("name required (60 characters at most)")
	}
	p := Plan{ID: id, Name: name, Position: u.Position, Public: u.Public,
		CPULimit: strings.TrimSpace(u.CPULimit), MemoryLimit: strings.TrimSpace(u.MemoryLimit), StorageLimit: strings.TrimSpace(u.StorageLimit),
		Fees: map[string]int64{}}
	for _, q := range []string{p.CPULimit, p.MemoryLimit, p.StorageLimit} {
		v, err := resource.ParseQuantity(q)
		if err != nil || v.Sign() <= 0 {
			return Plan{}, bad("limits must be positive Kubernetes quantities (e.g. 4000m, 8Gi)")
		}
	}
	for cur, fee := range u.Fees {
		c := NormalizeCurrency(cur)
		if c == "" || fee < 0 {
			return Plan{}, bad("fees: ISO currency codes and amounts of zero or more")
		}
		p.Fees[c] = fee
	}
	return p, nil
}

// Admin : vue d'administration.
func (p Plan) Admin() AdminPlan {
	return AdminPlan{ID: p.ID, Name: p.Name, Position: p.Position, Public: p.Public, CPULimit: p.CPULimit,
		MemoryLimit: p.MemoryLimit, StorageLimit: p.StorageLimit, Fees: p.Fees}
}

// ProjectUsage : ce qu'un projet réserve (mêmes règles que les quotas).
type ProjectUsage struct {
	ProjectID uuid.UUID
	Name      string
	CPUMilli  int64
	MemMi     int64
}

// ── Requêtes ────────────────────────────────────────────────────────────

// PlansRequest : plans publics dans une monnaie.
type PlansRequest struct {
	Currency string `query:"currency" description:"Monnaie (XAF ou XOF ; défaut XAF)"`
}

// ChangePlanRequest : changer de plan.
type ChangePlanRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Plan           string `json:"plan" binding:"required" description:"Identifiant du plan choisi"`
}

// PlanPath : un plan (administration).
type PlanPath struct {
	ID string `path:"planID" description:"Identifiant du plan"`
}

// LimitTarget : limites qu'un projet doit avoir (exception de l'équipe, sinon
// plan de son organisation) et celles qu'il a.
type LimitTarget struct {
	ProjectID            uuid.UUID
	OrganizationID       uuid.UUID
	Name                 string
	CPU, Memory, Storage string
	Override             bool
}

// NotApplied : projet resté à ses anciennes limites (il réserve déjà plus).
type NotApplied struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

// SavePlanResponse : plan enregistré et projets qui n'ont pas encore suivi
// (le rapprochement réessaie à chaque passage).
type SavePlanResponse struct {
	Plan       AdminPlan    `json:"plan"`
	Applied    int          `json:"applied"`
	NotApplied []NotApplied `json:"not_applied"`
}

// ProjectLimitsPath : un projet (exception de limites).
type ProjectLimitsPath struct {
	ProjectID string `path:"id" description:"Identifiant du projet"`
}
