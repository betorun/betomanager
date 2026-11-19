// Package domain : revente de noms de domaine (ADR 0012, phase 5). Le client
// achète depuis la console, payé par son solde ; la plateforme commande chez
// le registraire, le client en est le titulaire. Aucune dépendance vers la
// base, le registraire ou HTTP.
package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// États d'une commande.
const (
	StatusOrdering = "ordering" // payée par le client, à commander
	StatusPlaced   = "placed"   // commandée chez le registraire, en création
	StatusActive   = "active"   // domaine détenu
	StatusFailed   = "failed"   // non obtenu, client remboursé
	StatusExpired  = "expired"  // non renouvelé
)

// RenewBefore : le renouvellement est débité ce délai avant l'expiration.
const RenewBefore = 30 * 24 * time.Hour

var (
	ErrInvalidDomain  = errors.New("invalid domain name: e.g. mon-site.com")
	ErrNotAvailable   = errors.New("this domain is not available")
	ErrPremium        = errors.New("premium domains are not sold here")
	ErrNoPrice        = errors.New("this domain cannot be priced right now")
	ErrInvalidContact = errors.New("invalid owner details: first and last name, email, international phone, address, city and two-letter country are required")
	ErrOrderNotFound  = errors.New("domain order not found")
	ErrNotActive      = errors.New("this domain is not active yet")
	ErrSalesDisabled  = errors.New("domain sales are not open on this platform yet")
	ErrPriceChanged   = errors.New("the price changed: search again")
	// ErrUnavailable : le service des noms de domaine ne répond pas. Marque
	// blanche : jamais le message du registraire, seulement celui-ci.
	ErrUnavailable = errors.New("the domain name service is not responding: try again in a few minutes")
)

var (
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,24}$`)
	emailRe  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	phoneRe  = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
)

// NormalizeDomain : "Mon-Site.COM " → "mon-site.com", ou "".
func NormalizeDomain(raw string) string {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d = strings.TrimPrefix(d, "www.")
	if len(d) > 253 || !domainRe.MatchString(d) {
		return ""
	}
	return d
}

// Contact : titulaire du domaine (le client, règles des registres).
type Contact struct {
	FirstName    string `json:"first_name" description:"Prénom du titulaire"`
	LastName     string `json:"last_name" description:"Nom du titulaire"`
	Organization string `json:"organization,omitempty" description:"Organisation (facultatif ; le domaine lui appartient alors)"`
	Email        string `json:"email" description:"E-mail du titulaire"`
	Phone        string `json:"phone" description:"Téléphone au format international (+242…)"`
	Address      string `json:"address" description:"Adresse"`
	City         string `json:"city" description:"Ville"`
	PostalCode   string `json:"postal_code,omitempty" description:"Code postal (facultatif)"`
	Country      string `json:"country" description:"Pays (code à deux lettres, ex. CG)"`
}

// Normalize : titulaire nettoyé et validé.
func (c Contact) Normalize() (Contact, error) {
	t := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	out := Contact{FirstName: t(c.FirstName, 60), LastName: t(c.LastName, 60), Organization: t(c.Organization, 100),
		Email: strings.ToLower(t(c.Email, 200)), Address: t(c.Address, 200), City: t(c.City, 80), PostalCode: t(c.PostalCode, 20),
		Country: strings.ToUpper(t(c.Country, 2))}
	out.Phone = strings.NewReplacer(" ", "", "-", "", ".", "").Replace(strings.TrimSpace(c.Phone))
	if strings.HasPrefix(out.Phone, "00") {
		out.Phone = "+" + out.Phone[2:]
	}
	if out.FirstName == "" || out.LastName == "" || !emailRe.MatchString(out.Email) || !phoneRe.MatchString(out.Phone) ||
		out.Address == "" || out.City == "" || len(out.Country) != 2 {
		return Contact{}, ErrInvalidContact
	}
	return out, nil
}

// Order : une commande (puis un domaine détenu).
type Order struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrganizationID    uuid.UUID `gorm:"type:uuid;not null"`
	Domain            string    `gorm:"not null"`
	Status            string    `gorm:"not null"`
	Currency          string    `gorm:"not null"`
	Price             int64     `gorm:"not null"` // millièmes
	RenewPrice        int64     `gorm:"not null"` // millièmes
	RegistrarCents    int64     `gorm:"not null"`
	RenewCents        int64     `gorm:"not null"`
	RegistrarCurrency string    `gorm:"not null"`
	Contact           []byte    `gorm:"type:jsonb;not null"`
	RegistrarOrder    string    `gorm:"not null"`
	AutoRenew         bool      `gorm:"not null"`
	ExpiresAt         *time.Time
	PaidUntil         *time.Time
	RenewFailedAt     *time.Time
	Error             string `gorm:"not null"`
	CreatedBy         string `gorm:"not null"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ActiveAt          *time.Time
}

func (Order) TableName() string { return "domain_orders" }

// References des écritures du grand livre.
func PurchaseReference(o uuid.UUID) string { return "domain:" + o.String() }
func RefundReference(o uuid.UUID) string   { return "domain-refund:" + o.String() }
func RenewReference(o uuid.UUID, until time.Time) string {
	return "domain-renew:" + o.String() + ":" + until.UTC().Format("2006-01-02")
}

// Quote : réponse à une recherche.
type Quote struct {
	Domain    string `json:"domain"`
	Available bool   `json:"available"`
	// Price / RenewPrice : prix de vente par an, en unités de la monnaie.
	Price      int64  `json:"price,omitempty"`
	RenewPrice int64  `json:"renew_price,omitempty"`
	Currency   string `json:"currency"`
	// Reason : pourquoi on ne peut pas l'acheter (taken, premium, unpriced).
	Reason string `json:"reason,omitempty"`
	// Ordering : l'achat est ouvert (sinon recherche seulement).
	Ordering bool `json:"ordering"`
}

// OrderView : une commande vue par le client.
type OrderView struct {
	ID         string     `json:"id"`
	Domain     string     `json:"domain"`
	Status     string     `json:"status"`
	Price      int64      `json:"price"`
	RenewPrice int64      `json:"renew_price"`
	Currency   string     `json:"currency"`
	AutoRenew  bool       `json:"auto_renew"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// RenewFailed : le renouvellement n'a pas pu être débité (solde).
	RenewFailed bool      `json:"renew_failed,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// View : vue client (prix en unités).
func (o Order) View() OrderView {
	return OrderView{ID: o.ID.String(), Domain: o.Domain, Status: o.Status, Price: (o.Price + 500) / 1000,
		RenewPrice: (o.RenewPrice + 500) / 1000, Currency: o.Currency, AutoRenew: o.AutoRenew, ExpiresAt: o.ExpiresAt,
		RenewFailed: o.RenewFailedAt != nil, CreatedAt: o.CreatedAt}
}

// OrdersResponse : domaines de l'organisation.
type OrdersResponse struct {
	Domains  []OrderView `json:"domains"`
	Ordering bool        `json:"ordering"`
}

// ── Requêtes ────────────────────────────────────────────────────────────

// OrgPath : organisation.
type OrgPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
}

// SearchRequest : disponibilité et prix d'un nom.
type SearchRequest struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	Domain         string `query:"domain" description:"Nom recherché (ex. mon-site.com)"`
}

// BuyRequest : acheter un domaine.
type BuyRequest struct {
	OrganizationID string  `path:"id" json:"-" description:"Identifiant de l'organisation"`
	Domain         string  `json:"domain" binding:"required" description:"Nom à acheter"`
	Price          int64   `json:"price" binding:"required" description:"Prix annoncé à la recherche (refusé s'il a changé)"`
	Owner          Contact `json:"owner" description:"Titulaire du domaine (le client)"`
	AutoRenew      bool    `json:"auto_renew" description:"Renouveler chaque année (débité 30 jours avant l'expiration)"`
}

// OrderPath : une commande.
type OrderPath struct {
	OrganizationID string `path:"id" description:"Identifiant de l'organisation"`
	OrderID        string `path:"orderID" description:"Identifiant du domaine"`
}

// AttachRequest : brancher le domaine sur un conteneur.
type AttachRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	OrderID        string `path:"orderID" json:"-" description:"Identifiant du domaine"`
	WorkloadID     string `json:"workload_id" binding:"required" description:"Conteneur (web) qui répond sur ce domaine"`
	WWW            bool   `json:"www" description:"Brancher aussi www.<domaine>"`
}

// AutoRenewRequest : activer ou couper le renouvellement.
type AutoRenewRequest struct {
	OrganizationID string `path:"id" json:"-" description:"Identifiant de l'organisation"`
	OrderID        string `path:"orderID" json:"-" description:"Identifiant du domaine"`
	Enabled        bool   `json:"enabled" description:"Renouveler chaque année"`
}

// AttachResponse : noms branchés.
type AttachResponse struct {
	Hostnames []string `json:"hostnames"`
	// Verified : le routage est déjà vérifié (sinon il le sera dans les minutes).
	Verified bool `json:"verified"`
}
