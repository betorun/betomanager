package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Seuils d'alerte de solde (ADR 0012, décision 13), en jours de consommation
// restants au rythme actuel. Ce sont des règles, pas des montants.
const (
	LowBalanceDays      = 7
	CriticalBalanceDays = 2
)

// États d'un wallet.
const (
	WalletEstimate = "estimate" // facturation réelle pas encore activée
	WalletOK       = "ok"
	WalletLow      = "low"      // moins de 7 jours
	WalletCritical = "critical" // moins de 2 jours
	WalletEmpty    = "empty"    // solde nul ou négatif
)

// Niveaux d'alerte envoyés (billing_accounts.alert_level).
const (
	AlertNone     = 0
	AlertLow      = 1
	AlertCritical = 2
	AlertEmpty    = 3
)

// WalletStatus : état du solde au rythme de consommation quotidien.
func WalletStatus(charging bool, balance, perDay Milli) string {
	switch {
	case !charging:
		return WalletEstimate
	case balance <= 0:
		return WalletEmpty
	case perDay <= 0:
		return WalletOK
	case balance < perDay*CriticalBalanceDays:
		return WalletCritical
	case balance < perDay*LowBalanceDays:
		return WalletLow
	default:
		return WalletOK
	}
}

// AlertLevel : niveau d'alerte d'un état.
func AlertLevel(status string) int {
	switch status {
	case WalletLow:
		return AlertLow
	case WalletCritical:
		return AlertCritical
	case WalletEmpty:
		return AlertEmpty
	default:
		return AlertNone
	}
}

// CreditView : un crédit offert encore actif.
type CreditView struct {
	Amount    int64     `json:"amount"`
	Remaining int64     `json:"remaining"`
	ExpiresAt time.Time `json:"expires_at"`
	Reason    string    `json:"reason,omitempty"`
}

// Wallet : solde d'une organisation. Montants en unités de la monnaie.
type Wallet struct {
	Currency      string       `json:"currency"`
	Charging      bool         `json:"charging"`
	ChargingSince *time.Time   `json:"charging_since,omitempty"`
	Cash          int64        `json:"cash"`
	Credit        int64        `json:"credit"`
	Balance       int64        `json:"balance"`
	Credits       []CreditView `json:"credits"`
	// PerDay : consommation actuelle sur 24 h (heure en cours × 24, offre
	// gratuite non déduite : estimation prudente).
	PerDay int64 `json:"per_day"`
	// DaysLeft : jours restants à ce rythme (absent sans consommation).
	DaysLeft *int64 `json:"days_left,omitempty"`
	Status   string `json:"status"`
	// TopupMin / TopupMax : bornes d'une recharge (0 : recharges fermées).
	TopupMin int64 `json:"topup_min"`
	TopupMax int64 `json:"topup_max"`
	// GraceUntil : fin de la grâce (solde épuisé) ; SuspendedAt : conteneurs
	// arrêtés faute de solde, jusqu'à la prochaine recharge.
	GraceUntil  *time.Time `json:"grace_until,omitempty"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// Postpaid : présent en postpayé (plafond, dette, factures à payer).
	Postpaid *PostpaidView `json:"postpaid,omitempty"`
}

// DaysLeft : jours entiers restants, ou nil sans consommation.
func DaysLeft(balance, perDay Milli) *int64 {
	if perDay <= 0 {
		return nil
	}
	d := int64(0)
	if balance > 0 {
		d = int64(balance / perDay)
	}
	return &d
}

// TransactionView : un mouvement vu par le client (effet sur son solde).
type TransactionView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Description string    `json:"description,omitempty"`
	Period      string    `json:"period,omitempty"`
	Amount      int64     `json:"amount"` // effet sur le solde (argent + crédits)
	Cash        int64     `json:"cash"`
	Credit      int64     `json:"credit"`
	CreatedAt   time.Time `json:"created_at"`
}

// TransactionsResponse : page d'historique.
type TransactionsResponse struct {
	Transactions []TransactionView `json:"transactions"`
	// NextBefore : à repasser en before pour la page suivante.
	NextBefore string `json:"next_before,omitempty"`
}

// ── Recharges ───────────────────────────────────────────────────────────

// États d'une recharge.
const (
	TopupPending   = "pending"
	TopupSucceeded = "succeeded"
	TopupFailed    = "failed"
	TopupExpired   = "expired"
)

// TopupExpiry : une recharge non confirmée sur le téléphone expire.
const TopupExpiry = 15 * time.Minute

var (
	ErrTopupsClosed     = errors.New("top-ups are not open for this currency")
	ErrTopupAmount      = errors.New("top-up amount out of range")
	ErrInvalidPhone     = errors.New("invalid phone number: international format, e.g. +242061234567")
	ErrInvalidOperator  = errors.New("invalid mobile money operator")
	ErrPaymentsDisabled = errors.New("online payments are not configured on this platform")
	ErrChargingDisabled = errors.New("billing is not active for this organization yet: nothing to top up")
	ErrTopupNotFound    = errors.New("top-up not found")
	ErrTopupPending     = errors.New("a top-up is already waiting for confirmation on a phone")
	ErrInvalidCursor    = errors.New("invalid cursor")
	ErrBadWebhook       = errors.New("invalid webhook signature")
	ErrPaymentRefused   = errors.New("the payment was refused")
)

// TopupAmountError : montant hors des bornes de la grille.
type TopupAmountError struct {
	Min, Max int64
	Currency string
}

func (e *TopupAmountError) Error() string {
	return fmt.Sprintf("top-up amount out of range: between %d and %d %s", e.Min, e.Max, e.Currency)
}
func (e *TopupAmountError) Unwrap() error { return ErrTopupAmount }

// PaymentRefusedError : refus du prestataire de paiement (code de XYKPay).
type PaymentRefusedError struct{ Code string }

func (e *PaymentRefusedError) Error() string { return "the payment was refused: " + e.Code }
func (e *PaymentRefusedError) Unwrap() error { return ErrPaymentRefused }

var (
	phoneRe    = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	operatorRe = regexp.MustCompile(`^[a-z]{2}_[a-z0-9]{2,20}$`)
)

// NormalizePhone : "+242 06 123 45 67" → "+242061234567", ou "".
func NormalizePhone(raw string) string {
	p := strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(strings.TrimSpace(raw))
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	}
	if !phoneRe.MatchString(p) {
		return ""
	}
	return p
}

// ValidOperator : code d'opérateur XYKPay (ex. cg_mtn, cg_airtel).
func ValidOperator(op string) bool { return operatorRe.MatchString(op) }

// Topup : une recharge.
type Topup struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null"`
	Amount         Milli      `gorm:"not null"`
	Currency       string     `gorm:"not null"`
	Phone          string     `gorm:"not null"`
	Operator       string     `gorm:"not null"`
	Status         string     `gorm:"not null"`
	PaymentID      *string    `gorm:"column:payment_id"`
	FailureCode    string     `gorm:"not null;default:''"`
	Environment    string     `gorm:"not null;default:''"`
	TransactionID  *uuid.UUID `gorm:"type:uuid"`
	CreatedBy      string     `gorm:"not null;default:''"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SettledAt      *time.Time
}

func (Topup) TableName() string { return "billing_topups" }

// Done : la recharge ne changera plus.
func (t Topup) Done() bool { return t.Status != TopupPending }

// TopupView : recharge vue par le client (numéro masqué).
type TopupView struct {
	ID          string     `json:"id"`
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Phone       string     `json:"phone"`
	Operator    string     `json:"operator"`
	Status      string     `json:"status"`
	FailureCode string     `json:"failure_code,omitempty"`
	Test        bool       `json:"test,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SettledAt   *time.Time `json:"settled_at,omitempty"`
}

// MaskPhone : "+242061234567" → "+242 •••• 4567".
func MaskPhone(p string) string {
	if len(p) < 8 {
		return "••••"
	}
	return p[:4] + " •••• " + p[len(p)-4:]
}

// View : vue client.
func (t Topup) View() TopupView {
	return TopupView{ID: t.ID.String(), Amount: t.Amount.Units(), Currency: t.Currency, Phone: MaskPhone(t.Phone),
		Operator: t.Operator, Status: t.Status, FailureCode: t.FailureCode, Test: t.Environment == "sandbox",
		CreatedAt: t.CreatedAt, SettledAt: t.SettledAt}
}

// TopupsResponse : recharges récentes.
type TopupsResponse struct {
	Topups []TopupView `json:"topups"`
}

// ── Requêtes ────────────────────────────────────────────────────────────

// TopupRequest : recharger le wallet d'une organisation.
type TopupRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Amount         int64  `json:"amount" binding:"required" description:"Montant, en unités de la monnaie du compte"`
	Phone          string `json:"phone" binding:"required" description:"Numéro Mobile Money, au format international (+242…)"`
	Operator       string `json:"operator" binding:"required" description:"Opérateur Mobile Money (ex. cg_mtn, cg_airtel)"`
}

// TopupPath : une recharge.
type TopupPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	TopupID        string `path:"topupID" description:"Identifiant de la recharge"`
}

// TransactionsRequest : historique paginé.
type TransactionsRequest struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	Before         string `query:"before" description:"Curseur de la page suivante (next_before)"`
	Limit          int    `query:"limit" description:"Nombre de mouvements (défaut 50, au plus 200)"`
}
