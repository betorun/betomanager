package domain

import (
	"errors"
	"time"
)

// Administration de la facturation (équipe de la plateforme). Chaque geste
// passe par le grand livre, avec un motif ; l'audit garde qui l'a fait.

var (
	ErrReasonRequired       = errors.New("a reason is required")
	ErrInvalidAdminAmount   = errors.New("invalid amount")
	ErrOrganizationNotFound = errors.New("organization not found")
)

// OrgSummary : une organisation dans la liste de l'administration.
type OrgSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Owner    string `json:"owner,omitempty"`
	Projects int    `json:"projects"`
	// MaxProjects : plafond de projets (changé par PATCH /admin/organizations/:id/limits).
	MaxProjects int    `json:"max_projects"`
	Currency    string `json:"currency"`
	Charging    bool   `json:"charging"`
	Balance     int64  `json:"balance"`
	// MonthEstimate : coût estimé du mois en cours (après offre gratuite).
	MonthEstimate int64     `json:"month_estimate"`
	CreatedAt     time.Time `json:"created_at"`
}

// OrgsResponse : page de la liste.
type OrgsResponse struct {
	Organizations []OrgSummary `json:"organizations"`
}

// OrgProject : projet d'une organisation.
type OrgProject struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// OrgDetail : fiche d'une organisation pour l'équipe.
type OrgDetail struct {
	OrgSummary
	Projects     []OrgProject      `json:"project_list"`
	Wallet       Wallet            `json:"wallet"`
	Estimate     Estimate          `json:"estimate"`
	Transactions []TransactionView `json:"transactions"`
	Topups       []TopupView       `json:"topups"`
	// Plan : plan de l'organisation (absent si les plans ne sont pas branchés).
	Plan *PlanView `json:"plan,omitempty"`
}

// AdminOrgsRequest : recherche.
type AdminOrgsRequest struct {
	Query string `query:"q" description:"Recherche dans le nom de l'organisation, l'e-mail du propriétaire ou un nom de projet ; un identifiant d'organisation ou de projet exact"`
	Limit int    `query:"limit" description:"Nombre de résultats (défaut 50, au plus 200)"`
}

// AdminOrgPath : une organisation.
type AdminOrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

// CreditRequest : offrir un crédit.
type CreditRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Amount         int64  `json:"amount" binding:"required" description:"Montant offert, en unités de la monnaie du compte"`
	Days           int    `json:"days" binding:"required" description:"Validité, en jours"`
	Reason         string `json:"reason" binding:"required" description:"Motif (visible par l'équipe et le client)"`
}

// AdjustmentRequest : corriger l'argent d'un client (remboursement d'un
// trop-perçu, geste commercial, erreur de mesure). Positif : crédité ;
// négatif : débité.
type AdjustmentRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Amount         int64  `json:"amount" binding:"required" description:"Montant signé, en unités : positif crédite le client, négatif le débite"`
	Reason         string `json:"reason" binding:"required" description:"Motif (visible par l'équipe et le client)"`
}

// ChargingRequest : activer ou suspendre la facturation réelle.
type ChargingRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Enabled        bool   `json:"enabled" description:"true : l'usage est débité du wallet ; false : coût seulement estimé"`
}

// RevenueDay : recettes d'un jour, dans une monnaie.
type RevenueDay struct {
	Day      string `json:"day"`
	Currency string `json:"currency"`
	Topups   int64  `json:"topups"`
	Usage    int64  `json:"usage"`
	Credits  int64  `json:"credits"`
}

// RevenueResponse : recettes par jour.
type RevenueResponse struct {
	Days []RevenueDay `json:"days"`
	// PendingTopups / FailedTopups : sur la période.
	PendingTopups int `json:"pending_topups"`
	FailedTopups  int `json:"failed_topups"`
}

// RevenueRequest : période.
type RevenueRequest struct {
	Days int `query:"days" description:"Nombre de jours (défaut 30, au plus 366)"`
}
