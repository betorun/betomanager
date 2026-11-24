package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Phase 4 (ADR 0012) : grâce et suspension douce, factures et reçus,
// budgets par projet.

//  Grâce et suspension

// Raisons d'un arrêt de conteneur par la facturation.
const (
	StopBalance = "balance" // solde épuisé, grâce écoulée
	StopBudget  = "budget"  // budget mensuel du projet atteint
)

// GraceUntil : fin de la grâce commencée à start.
func GraceUntil(start time.Time, graceDays int) time.Time {
	return start.Add(time.Duration(graceDays) * 24 * time.Hour)
}

// ShouldSuspend : solde épuisé et grâce écoulée, ou dette déjà supérieure à
// graceDays jours de consommation (la grâce est plafonnée en jours ET en
// montant : une consommation énorme n'attend pas trois jours).
func ShouldSuspend(balance, perDay Milli, graceStart *time.Time, graceDays int, now time.Time) bool {
	if balance > 0 || graceStart == nil {
		return false
	}
	if !now.Before(GraceUntil(*graceStart, graceDays)) {
		return true
	}
	return perDay > 0 && -balance >= perDay*Milli(graceDays)
}

// WorkloadRef : un conteneur en marche (arrêté par la suspension ou le budget).
type WorkloadRef struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Name      string
}

// InvoiceLines : lignes d'une facture mensuelle, par nature de ressource
// (plafonds compris), puis l'offre gratuite en négatif. Leur somme égale
// NetCost : ce que le débit a pris.
func (c Catalog) InvoiceLines(totals []Total) []DocLine {
	labels := []struct {
		label  string
		meters []Meter
	}{
		{"Conteneurs", []Meter{MeterCompute}},
		{"Disques des conteneurs", []Meter{MeterDisk}},
		{"Bases de données", []Meter{MeterDatabase, MeterDatabaseDisk}},
		{"Minutes de build", []Meter{MeterBuildMinutes}},
		{"Domaines de site", []Meter{MeterSiteDomain}},
		{"IP de sortie dédiées", []Meter{MeterEgressIP}},
		{"Abonnement", []Meter{MeterPlan}},
	}
	sums := map[Meter]Milli{}
	var compute, database, build Milli
	for _, t := range totals {
		cost := t.Cost()
		sums[t.Meter] += cost
		switch t.Meter {
		case MeterCompute:
			compute += cost
		case MeterDatabase, MeterDatabaseDisk:
			database += cost
		case MeterBuildMinutes:
			build += cost
		}
	}
	var out []DocLine
	for _, l := range labels {
		var sum Milli
		for _, m := range l.meters {
			sum += sums[m]
		}
		if sum > 0 {
			out = append(out, DocLine{Label: l.label, Amount: sum})
		}
	}
	if free := c.freeTier(compute, database, build).total; free > 0 {
		out = append(out, DocLine{Label: "Offre gratuite du mois", Amount: -free})
	}
	return out
}

//  Profil de facturation

// Profile : informations portées sur les factures du client.
type Profile struct {
	LegalName string `json:"legal_name"`
	TaxID     string `json:"tax_id"`
	Address   string `json:"address"`
	Country   string `json:"country"`
}

// ProfileRequest : modifier le profil de facturation.
type ProfileRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	LegalName      string `json:"legal_name" description:"Raison sociale (ou nom) portée sur les factures"`
	TaxID          string `json:"tax_id" description:"Numéro d'identification fiscale (NIU…), facultatif"`
	Address        string `json:"address" description:"Adresse de facturation"`
	Country        string `json:"country" description:"Pays (code ISO à deux lettres, ex. CG)"`
}

var ErrInvalidProfile = errors.New("invalid billing profile: legal name required, country as a two-letter code")

// NormalizeProfile : profil nettoyé et validé.
func NormalizeProfile(r ProfileRequest) (Profile, error) {
	clip := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	p := Profile{LegalName: clip(r.LegalName, 200), TaxID: clip(r.TaxID, 60), Address: clip(r.Address, 400),
		Country: strings.ToUpper(strings.TrimSpace(r.Country))}
	if p.LegalName == "" || (p.Country != "" && len(p.Country) != 2) {
		return Profile{}, ErrInvalidProfile
	}
	return p, nil
}

//  Factures et reçus

// Natures de document.
const (
	DocInvoice = "invoice"
	DocReceipt = "receipt"
)

// Issuer : l'entité qui facture (configuration, pas de montant).
type Issuer struct {
	Name    string
	Address string
	TaxID   string
	Country string
	// VATRateBP : taux de TVA en points de base (1890 = 18,90 %). Les prix de
	// la grille sont TTC : la facture en extrait la TVA.
	VATRateBP int
	// PaymentInstructions : comment payer une facture postpayée (RIB…).
	PaymentInstructions string
	// InvoicePrefix / ReceiptPrefix : préfixes de numérotation.
	InvoicePrefix string
	ReceiptPrefix string
}

// Configured : de quoi émettre une facture conforme.
func (i Issuer) Configured() bool { return i.Name != "" && i.Address != "" }

// SplitVAT : part de TVA d'un montant TTC (arrondie au millième).
func SplitVAT(total Milli, rateBP int) Milli {
	if rateBP <= 0 || total <= 0 {
		return 0
	}
	// TVA = TTC × taux / (1 + taux), en entiers.
	return Milli((int64(total)*int64(rateBP) + int64(10000+rateBP)/2) / int64(10000+rateBP))
}

// DocumentNumber : "BTR-2026-000042".
func DocumentNumber(prefix string, year, n int) string {
	return fmt.Sprintf("%s-%d-%06d", prefix, year, n)
}

// DocLine : une ligne de facture (montants TTC, en millièmes).
type DocLine struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	Amount Milli  `json:"amount"`
}

// DocData : contenu figé d'un document (repris à l'identique dans le PDF).
type DocData struct {
	Issuer   Issuer    `json:"issuer"`
	Customer Profile   `json:"customer"`
	OrgName  string    `json:"org_name"`
	Lines    []DocLine `json:"lines"`
	// Paid : réglé par crédits offerts / par le solde payé (facture).
	PaidByCredits Milli `json:"paid_by_credits"`
	PaidByCash    Milli `json:"paid_by_cash"`
	// Payment : moyen de paiement (reçu), ex. "MTN Mobile Money +242 •••• 4567".
	Payment string `json:"payment,omitempty"`
}

// Document : facture ou reçu émis (immuable).
type Document struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null"`
	Kind           string     `gorm:"not null"`
	Number         string     `gorm:"not null"`
	Period         string     `gorm:"not null;default:''"`
	TopupID        *uuid.UUID `gorm:"type:uuid"`
	Currency       string     `gorm:"not null"`
	Total          Milli      `gorm:"not null"`
	VAT            Milli      `gorm:"column:vat;not null"`
	VATRateBP      int        `gorm:"column:vat_rate_bp;not null"`
	Data           []byte     `gorm:"type:jsonb;not null"`
	PDF            []byte     `gorm:"column:pdf;not null"`
	IssuedAt       time.Time  `gorm:"not null"`
	// DueAt : échéance (postpayé) ; nil : facture déjà réglée par le solde.
	DueAt *time.Time
}

func (Document) TableName() string { return "billing_documents" }

// DocumentView : document vu par le client (sans le PDF).
type DocumentView struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Number   string    `json:"number"`
	Period   string    `json:"period,omitempty"`
	Currency string    `json:"currency"`
	Total    int64     `json:"total"`
	VAT      int64     `json:"vat"`
	IssuedAt time.Time `json:"issued_at"`
}

// View : vue client.
func (d Document) View() DocumentView {
	return DocumentView{ID: d.ID.String(), Kind: d.Kind, Number: d.Number, Period: d.Period, Currency: d.Currency,
		Total: d.Total.Units(), VAT: d.VAT.Units(), IssuedAt: d.IssuedAt}
}

// DocumentsResponse : factures et reçus.
type DocumentsResponse struct {
	Documents []DocumentView `json:"documents"`
	// IssuerReady : faux tant que l'entité qui facture n'est pas configurée
	// (aucune facture n'est alors émise).
	IssuerReady bool `json:"issuer_ready"`
}

// DocumentPath : un document.
type DocumentPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	DocumentID     string `path:"documentID" description:"Identifiant de la facture ou du reçu"`
}

var ErrDocumentNotFound = errors.New("document not found")

//  Budgets par projet

// Seuils d'alerte d'un budget.
const (
	BudgetAlertNone = 0
	BudgetAlertNear = 1 // 80 %
	BudgetAlertOver = 2 // 100 %
	BudgetNearPct   = 80
)

// BudgetLevel : seuil atteint par spent sur limit.
func BudgetLevel(spent, limit Milli) int {
	switch {
	case limit <= 0:
		return BudgetAlertNone
	case spent >= limit:
		return BudgetAlertOver
	case spent*100 >= limit*BudgetNearPct:
		return BudgetAlertNear
	default:
		return BudgetAlertNone
	}
}

// Budget : budget mensuel d'un projet.
type Budget struct {
	ProjectID string `json:"project_id"`
	// Limit : montant mensuel, en unités (0 : pas de budget).
	Limit int64 `json:"limit"`
	// Stop : arrêter les conteneurs du projet une fois le budget atteint.
	Stop bool `json:"stop"`
	// Spent : coût du projet ce mois-ci (avant offre gratuite), en unités.
	Spent    int64  `json:"spent"`
	Currency string `json:"currency"`
	// Stopped : conteneurs arrêtés par le budget ce mois-ci.
	Stopped bool `json:"stopped"`
}

// BudgetRequest : fixer (limit > 0) ou retirer (limit = 0) un budget.
type BudgetRequest struct {
	ProjectID string `path:"id" json:"-" description:"Identifiant du projet"`
	Limit     int64  `json:"limit" description:"Budget mensuel en unités de la monnaie (0 : retirer le budget)"`
	Stop      bool   `json:"stop" description:"true : arrêter les conteneurs du projet une fois le budget atteint ; false : alerte seulement"`
}

// ProjectPath : un projet.
type ProjectPath struct {
	ProjectID string `path:"id" description:"Identifiant du projet"`
}

var ErrInvalidBudget = errors.New("invalid budget: a positive whole amount, or 0 to remove it")

//  Suspension (erreur vue par les autres modules)

// ErrAccountSuspended : l'organisation est suspendue faute de solde, ou le
// projet a atteint son budget avec arrêt : rien ne démarre avant une
// recharge (ou un budget relevé).
// ErrProjectPendingDeletion : projet en suppression programmée ; rien n'y
// démarre avant sa restauration.
var ErrProjectPendingDeletion = errors.New("this project is scheduled for deletion: restore it before starting anything")

var ErrAccountSuspended = errors.New("billing: this organization's balance is used up, or the project reached its budget: top up (or raise the budget) to start containers again")

// ErrProjectUnknown : le projet n'existe pas (ou plus) côté facturation. À
// distinguer d'une erreur de lecture, qui ne doit jamais ouvrir une garde.
var ErrProjectUnknown = errors.New("billing: unknown project")

// ErrBillingCheckUnavailable : la garde n'a pas pu lire la facturation
// (panne passagère de la base) ; le démarrage est refusé, à réessayer.
var ErrBillingCheckUnavailable = errors.New("the billing check is temporarily unavailable: try again in a moment")
