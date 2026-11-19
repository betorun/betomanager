package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Grand livre en partie double (ADR 0012, décision 11). Chaque transaction
// regroupe des écritures dont la somme est nulle ; rien ne se modifie, on
// corrige par une transaction inverse. La base le vérifie aussi (migration
// 000035) : une transaction déséquilibrée ne peut pas être validée.

// Comptes de la plateforme.
const (
	AccountRevenue     = "platform:revenue"     // usage payé en argent
	AccountPromotions  = "platform:promotions"  // crédits offerts (et leur part consommée)
	AccountXYKPay      = "platform:xykpay"      // argent reçu par XYKPay
	AccountAdjustments = "platform:adjustments" // gestes de l'équipe
	AccountVAT         = "platform:vat"         // TVA collectée (factures)
	AccountDomains     = "platform:domains"     // ventes de noms de domaine
)

// CashAccount : argent payé par le client (peut devenir négatif pendant la grâce).
func CashAccount(org uuid.UUID) string { return "customer:" + org.String() + ":cash" }

// CreditAccount : crédits offerts au client (jamais négatif).
func CreditAccount(org uuid.UUID) string { return "customer:" + org.String() + ":credit" }

// Natures de transaction.
const (
	TxTopup        = "topup"         // recharge reçue
	TxUsage        = "usage"         // débit de l'usage
	TxCreditGrant  = "credit_grant"  // crédit offert
	TxCreditExpiry = "credit_expiry" // reste d'un crédit expiré
	TxAdjustment   = "adjustment"    // geste de l'équipe (crédit ou débit d'argent)
	TxVAT          = "vat"           // TVA d'une facture, sortie du revenu
	TxDomain       = "domain"        // achat ou renouvellement d'un domaine
	TxDomainRefund = "domain_refund" // remboursement d'un domaine non obtenu
)

// ErrInsufficientFunds : solde payé insuffisant pour un achat.
var ErrInsufficientFunds = errors.New("insufficient balance: top up first")

var (
	ErrUnbalanced = errors.New("billing transaction is not balanced")
	ErrZeroAmount = errors.New("amount must not be zero")
)

// Transaction : en-tête d'un mouvement.
type Transaction struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrganizationID *uuid.UUID `gorm:"type:uuid"`
	Kind           string     `gorm:"not null"`
	Reference      string     `gorm:"not null"`
	Period         string     `gorm:"not null;default:''"`
	Currency       string     `gorm:"not null"`
	Description    string     `gorm:"not null;default:''"`
	CreatedBy      string     `gorm:"not null;default:''"`
	CreatedAt      time.Time
	Entries        []Entry `gorm:"foreignKey:TransactionID"`
}

func (Transaction) TableName() string { return "billing_transactions" }

// Entry : une écriture (montant signé, en millièmes).
type Entry struct {
	ID             int64      `gorm:"primaryKey"`
	TransactionID  uuid.UUID  `gorm:"type:uuid;not null"`
	Account        string     `gorm:"not null"`
	OrganizationID *uuid.UUID `gorm:"type:uuid"`
	Amount         Milli      `gorm:"not null"`
	Currency       string     `gorm:"not null"`
	CreatedAt      time.Time
}

func (Entry) TableName() string { return "billing_ledger_entries" }

// NewTransaction : transaction équilibrée, prête à écrire. Les écritures
// nulles sont retirées ; il en faut au moins deux, de somme nulle.
func NewTransaction(org uuid.UUID, kind, reference, currency, description, by string, entries map[string]Milli, now time.Time) (*Transaction, error) {
	tx := &Transaction{ID: uuid.New(), Kind: kind, Reference: reference, Currency: currency,
		Description: description, CreatedBy: by, CreatedAt: now}
	if org != uuid.Nil {
		o := org
		tx.OrganizationID = &o
	}
	var sum Milli
	for account, amount := range entries {
		if amount == 0 {
			continue
		}
		sum += amount
		e := Entry{TransactionID: tx.ID, Account: account, Amount: amount, Currency: currency, CreatedAt: now}
		if org != uuid.Nil {
			o := org
			e.OrganizationID = &o
		}
		tx.Entries = append(tx.Entries, e)
	}
	if len(tx.Entries) == 0 {
		return nil, ErrZeroAmount
	}
	if sum != 0 || len(tx.Entries) < 2 {
		return nil, ErrUnbalanced
	}
	return tx, nil
}

// CreditLot : reste d'un crédit offert.
type CreditLot struct {
	ID        uuid.UUID
	Remaining Milli
	ExpiresAt time.Time
}

// Allocation : part d'un débit prise sur chaque crédit, puis sur l'argent.
type Allocation struct {
	FromCredits map[uuid.UUID]Milli
	Credit      Milli // total pris sur les crédits
	Cash        Milli // reste, pris sur l'argent (le solde peut devenir négatif)
}

// Allocate : un débit consomme d'abord les crédits encore valables, le plus
// proche de l'expiration d'abord (lots déjà triés), puis l'argent payé.
func Allocate(amount Milli, lots []CreditLot, now time.Time) Allocation {
	a := Allocation{FromCredits: map[uuid.UUID]Milli{}}
	left := amount
	for _, l := range lots {
		if left <= 0 {
			break
		}
		if l.Remaining <= 0 || !l.ExpiresAt.After(now) {
			continue
		}
		take := minMilli(left, l.Remaining)
		a.FromCredits[l.ID] += take
		a.Credit += take
		left -= take
	}
	a.Cash = left
	return a
}

// UsageEntries : écritures d'un débit d'usage. Côté plateforme, la part payée
// en argent est un revenu ; la part prise sur les crédits revient au compte
// des promotions (qui les avait offerts).
func UsageEntries(org uuid.UUID, a Allocation) map[string]Milli {
	return map[string]Milli{
		CreditAccount(org): -a.Credit,
		CashAccount(org):   -a.Cash,
		AccountPromotions:  a.Credit,
		AccountRevenue:     a.Cash,
	}
}

// Reference des transactions (clés d'idempotence).
func UsageReference(org uuid.UUID, hour time.Time) string {
	return "usage:" + org.String() + ":" + hour.UTC().Format("2006-01-02T15")
}

func TopupReference(topup uuid.UUID) string   { return "topup:" + topup.String() }
func TrialReference(org uuid.UUID) string     { return "trial:" + org.String() }
func ExpiryReference(grant uuid.UUID) string  { return "credit-expiry:" + grant.String() }
func GrantReference(grant uuid.UUID) string   { return "credit:" + grant.String() }
func AdjustmentReference(id uuid.UUID) string { return "adjustment:" + id.String() }
func VATReference(doc uuid.UUID) string       { return "vat:" + doc.String() }

// CreditGrant : un crédit offert (lot).
type CreditGrant struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null"`
	Amount         Milli     `gorm:"not null"`
	Remaining      Milli     `gorm:"not null"`
	Currency       string    `gorm:"not null"`
	ExpiresAt      time.Time `gorm:"not null"`
	Reason         string    `gorm:"not null;default:''"`
	TransactionID  uuid.UUID `gorm:"type:uuid;not null"`
	ExpiredAt      *time.Time
	CreatedBy      string `gorm:"not null;default:''"`
	CreatedAt      time.Time
}

func (CreditGrant) TableName() string { return "billing_credit_grants" }
