package domain

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Postpayé (ADR 0012, phase 5) : l'usage est débité à l'heure comme en
// prépayé ; le solde devient une dette, tolérée jusqu'au plafond de crédit.
// Chaque facture mensuelle est à payer avant son échéance. Un paiement
// (virement enregistré par l'équipe, ou recharge mobile money) réduit la
// dette ; les factures sont soldées de la plus ancienne à la plus récente.

// AccountBank : virements reçus (postpayé).
const AccountBank = "platform:bank"

// TxPayment : paiement reçu par virement (enregistré par l'équipe).
const TxPayment = "payment"

// PaymentReference : clé d'un paiement.
func PaymentReference(id uuid.UUID) string { return "payment:" + id.String() }

var (
	ErrInvalidPayment = errors.New("invalid payment: a positive amount and a reference")
)

// Invoice : facture mensuelle (pour le calcul des impayés).
type Invoice struct {
	ID       uuid.UUID
	Number   string
	Period   string
	Total    Milli
	IssuedAt time.Time
	DueAt    *time.Time
}

// ReceivableView : une facture et ce qu'il en reste à payer.
type ReceivableView struct {
	DocumentID string     `json:"document_id"`
	Number     string     `json:"number"`
	Period     string     `json:"period"`
	Total      int64      `json:"total"`
	Unpaid     int64      `json:"unpaid"`
	DueAt      *time.Time `json:"due_at,omitempty"`
	// DaysOverdue : jours de retard (0 : pas en retard).
	DaysOverdue int `json:"days_overdue"`
}

// Receivables : factures non soldées. La dette déjà facturée (outstanding)
// porte sur les factures les plus récentes : les paiements soldent les plus
// anciennes d'abord.
func Receivables(invoices []Invoice, outstanding Milli, now time.Time) []ReceivableView {
	sorted := append([]Invoice(nil), invoices...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].IssuedAt.After(sorted[j].IssuedAt) })
	out := []ReceivableView{}
	left := outstanding
	for _, inv := range sorted {
		if left <= 0 {
			break
		}
		unpaid := minMilli(left, inv.Total)
		left -= unpaid
		if unpaid <= 0 {
			continue
		}
		r := ReceivableView{DocumentID: inv.ID.String(), Number: inv.Number, Period: inv.Period, Total: inv.Total.Units(),
			Unpaid: unpaid.Units(), DueAt: inv.DueAt}
		if inv.DueAt != nil && now.After(*inv.DueAt) {
			r.DaysOverdue = int(now.Sub(*inv.DueAt) / (24 * time.Hour))
		}
		out = append(out, r)
	}
	// Plus ancienne d'abord (lecture naturelle).
	sort.Slice(out, func(i, j int) bool { return out[i].Period < out[j].Period })
	return out
}

// InvoicedDebt : part de la dette déjà facturée (le reste vient de l'usage
// du mois en cours, pas encore facturé).
func InvoicedDebt(cash, uninvoicedUsage Milli) Milli {
	if cash >= 0 {
		return 0
	}
	return max(0, -cash-uninvoicedUsage)
}

// PostpaidTerms : réglages postpayés d'un compte (défauts de la grille).
type PostpaidTerms struct {
	CreditLimit  Milli
	TermsDays    int
	ReminderDays int
	SuspendDays  int
}

// PostpaidView : état du postpayé (wallet).
type PostpaidView struct {
	CreditLimit int64 `json:"credit_limit"`
	TermsDays   int   `json:"terms_days"`
	// Outstanding : dette (usage non payé) ; Available : ce qui reste avant le plafond.
	Outstanding int64            `json:"outstanding"`
	Available   int64            `json:"available"`
	Invoices    []ReceivableView `json:"invoices"`
}

// PostpaidRequest : passer une organisation en postpayé, ou la ramener au
// prépayé (équipe). CreditLimit / TermsDays absents : défauts de la grille.
type PostpaidRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Enabled        bool   `json:"enabled" description:"true : postpayé (factures à échéance) ; false : prépayé"`
	CreditLimit    *int64 `json:"credit_limit,omitempty" description:"Plafond de dette, en unités (défaut : grille)"`
	TermsDays      *int   `json:"terms_days,omitempty" description:"Échéance des factures, en jours (défaut : grille)"`
}

// PaymentRequest : paiement reçu par virement.
type PaymentRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Amount         int64  `json:"amount" binding:"required" description:"Montant reçu, en unités"`
	Reference      string `json:"reference" binding:"required" description:"Référence du virement (relevé bancaire)"`
}
