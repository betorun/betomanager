package registrar

import (
	"context" //nolint:gosec // imposé par la signature de l'API OVH
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
)

// OVHConfig : identifiants de l'API OVH (https://api.ovh.com). Secrets, hors
// du code. La clé client (ConsumerKey) doit être limitée aux chemins /order et
// /domain.
type OVHConfig struct {
	Endpoint          string // défaut : https://eu.api.ovh.com/1.0
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
	// Subsidiary : filiale OVH de la commande (ex. FR), qui fixe la
	// devise et les extensions disponibles.
	Subsidiary string
	// OrderingEnabled : commandes réelles permises. Faux : Register refuse
	// (recherche et prix restent disponibles).
	OrderingEnabled bool
}

const (
	ovhPriceTotal   = "TOTAL"
	ovhPriceRenew   = "RENEW"
	ovhCentsPerUnit = 100
)

// OVH : revente de domaines par l'API OVH (client signé commun : infrastructure/ovh).
type OVH struct {
	cfg OVHConfig
	api *ovh.Client
}

// NewOVH : client ; erreur si un identifiant manque.
func NewOVH(cfg OVHConfig) (*OVH, error) {
	if cfg.Subsidiary == "" {
		return nil, errors.New("registrar: incomplete OVH configuration")
	}
	api, err := ovh.New(ovh.Credentials{Endpoint: cfg.Endpoint, ApplicationKey: cfg.ApplicationKey,
		ApplicationSecret: cfg.ApplicationSecret, ConsumerKey: cfg.ConsumerKey})
	if err != nil {
		return nil, errors.New("registrar: incomplete OVH configuration")
	}
	return &OVH{cfg: cfg, api: api}, nil
}

func (o *OVH) call(ctx context.Context, method, path string, in, out any) error {
	return o.api.Call(ctx, method, path, in, out)
}

type ovhCart struct {
	CartID string `json:"cartId"`
}

type ovhPrice struct {
	Label string `json:"label"`
	Price struct {
		Value        float64 `json:"value"`
		CurrencyCode string  `json:"currencyCode"`
	} `json:"price"`
}

type ovhOffer struct {
	Orderable bool       `json:"orderable"`
	Action    string     `json:"action"`
	Offer     string     `json:"offer"`
	Prices    []ovhPrice `json:"prices"`
}

func (o *OVH) newCart(ctx context.Context) (string, error) {
	var cart ovhCart
	err := o.call(ctx, http.MethodPost, "/order/cart", map[string]string{"ovhSubsidiary": o.cfg.Subsidiary}, &cart)
	return cart.CartID, err
}

// Check : disponibilité et prix, via un panier jetable (aucune commande).
func (o *OVH) Check(ctx context.Context, domain string) (*Availability, error) {
	cart, err := o.newCart(ctx)
	if err != nil {
		return nil, err
	}
	return o.checkIn(ctx, cart, domain)
}

// checkConcurrency : appels simultanés au plus pour une vérification groupée
// (le fournisseur limite le débit de son API).
const checkConcurrency = 6

// CheckMany : un seul panier pour tous les noms, vérifiés en parallèle.
func (o *OVH) CheckMany(ctx context.Context, domains []string) map[string]*Availability {
	out := make(map[string]*Availability, len(domains))
	cart, err := o.newCart(ctx)
	if err != nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, checkConcurrency)
	for _, d := range domains {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if a, err := o.checkIn(ctx, cart, d); err == nil {
				mu.Lock()
				out[d] = a
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return out
}

func (o *OVH) checkIn(ctx context.Context, cart, domain string) (*Availability, error) {
	var offers []ovhOffer
	path := "/order/cart/" + url.PathEscape(cart) + "/domain?domain=" + url.QueryEscape(domain)
	if err := o.call(ctx, http.MethodGet, path, nil, &offers); err != nil {
		return nil, err
	}
	out := &Availability{Domain: domain, Unsupported: len(offers) == 0}
	for _, offer := range offers {
		if !offer.Orderable || offer.Action != "create" {
			continue
		}
		out.Available = true
		out.Premium = offer.Offer == "premium"
		for _, p := range offer.Prices {
			switch p.Label {
			case ovhPriceTotal:
				out.PriceCents = cents(p.Price.Value)
				out.Currency = p.Price.CurrencyCode
			case ovhPriceRenew:
				out.RenewCents = cents(p.Price.Value)
			}
		}
		break
	}
	if out.Available && out.RenewCents == 0 {
		out.RenewCents = out.PriceCents
	}
	return out, nil
}

func cents(v float64) int64 { return int64(v*ovhCentsPerUnit + 0.5) }

// Register : commande réelle, payée par le moyen de paiement par défaut du
// compte OVH. Le titulaire est le client (contact créé pour lui). Jamais de
// nouvel essai automatique : l'appelant garde l'identifiant de commande et
// suit son état (OrderStatus) avant toute autre tentative.
func (o *OVH) Register(ctx context.Context, order Order) (*OrderResult, error) {
	if !o.cfg.OrderingEnabled {
		return nil, ErrOrderingDisabled
	}
	cart, err := o.newCart(ctx)
	if err != nil {
		return nil, err
	}
	base := "/order/cart/" + url.PathEscape(cart)
	years := max(order.Years, 1)
	var item struct {
		ItemID int64      `json:"itemId"`
		Prices []ovhPrice `json:"prices"`
	}
	if err := o.call(ctx, http.MethodPost, base+"/domain", map[string]any{"domain": order.Domain, "duration": fmt.Sprintf("P%dY", years)}, &item); err != nil {
		return nil, err
	}
	contactID, err := o.createContact(ctx, order.Owner)
	if err != nil {
		return nil, err
	}
	itemPath := base + "/item/" + strconv.FormatInt(item.ItemID, 10) + "/configuration"
	if err := o.call(ctx, http.MethodPost, itemPath, map[string]any{"label": "OWNER_CONTACT", "value": "/me/contact/" + strconv.FormatInt(contactID, 10)}, nil); err != nil {
		return nil, err
	}
	if err := o.call(ctx, http.MethodPost, base+"/assign", nil, nil); err != nil {
		return nil, err
	}
	var res struct {
		OrderID int64 `json:"orderId"`
		Prices  struct {
			WithTax struct {
				Value float64 `json:"value"`
			} `json:"withTax"`
		} `json:"prices"`
	}
	if err := o.call(ctx, http.MethodPost, base+"/checkout",
		map[string]any{"autoPayWithPreferredPaymentMethod": true, "waiveRetractationPeriod": true}, &res); err != nil {
		return nil, err
	}
	if res.OrderID == 0 {
		return nil, errors.New("registrar: OVH checkout returned no order")
	}
	return &OrderResult{OrderID: strconv.FormatInt(res.OrderID, 10), PriceCents: cents(res.Prices.WithTax.Value)}, nil
}

// createContact : titulaire du domaine (le client), dans le compte OVH.
func (o *OVH) createContact(ctx context.Context, c Contact) (int64, error) {
	legal := "individual"
	if c.Organization != "" {
		legal = "corporation"
	}
	in := map[string]any{
		"firstName": c.FirstName, "lastName": c.LastName, "email": c.Email, "phone": ovhPhone(c.Phone),
		"legalForm": legal, "language": "fr_FR",
		"address": map[string]any{"line1": c.Address, "city": c.City, "zip": c.PostalCode, "country": strings.ToUpper(c.Country)},
	}
	if c.Organization != "" {
		in["organisationName"] = c.Organization
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := o.call(ctx, http.MethodPost, "/me/contact", in, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// ovhPhone : "+242061234567" → "+242.061234567" (format OVH, indicatif séparé).
func ovhPhone(p string) string {
	for _, n := range []int{3, 2, 1} {
		if len(p) > n+1 && p[0] == '+' {
			if _, ok := callingCodes[p[1:1+n]]; ok {
				return p[:1+n] + "." + p[1+n:]
			}
		}
	}
	return p
}

// callingCodes : indicatifs des pays servis (et de quelques autres).
var callingCodes = map[string]struct{}{
	"242": {}, "243": {}, "237": {}, "241": {}, "236": {}, "235": {}, "240": {}, // CEMAC, RDC
	"221": {}, "223": {}, "225": {}, "226": {}, "227": {}, "228": {}, "229": {}, "245": {}, // UEMOA
	"33": {}, "32": {}, "41": {}, "1": {}, "44": {}, "49": {},
}

// OrderStatus : état d'une commande OVH.
func (o *OVH) OrderStatus(ctx context.Context, orderID string) (string, error) {
	var status string
	if err := o.call(ctx, http.MethodGet, "/me/order/"+url.PathEscape(orderID)+"/status", nil, &status); err != nil {
		return "", err
	}
	switch status {
	case "delivered":
		return OrderDelivered, nil
	case "cancelled", "notPaid":
		return OrderFailed, nil
	default: // checking, delivering, documentsRequested…
		return OrderPending, nil
	}
}

// Info : expiration et renouvellement d'un domaine du compte.
func (o *OVH) Info(ctx context.Context, domain string) (*DomainInfo, error) {
	var si struct {
		Expiration string `json:"expiration"`
		Renew      struct {
			Automatic bool `json:"automatic"`
		} `json:"renew"`
	}
	if err := o.call(ctx, http.MethodGet, "/domain/"+url.PathEscape(domain)+"/serviceInfos", nil, &si); err != nil {
		return nil, err
	}
	exp, err := time.Parse("2006-01-02", si.Expiration)
	if err != nil {
		return nil, fmt.Errorf("registrar: OVH expiration %q: %w", si.Expiration, err)
	}
	return &DomainInfo{Expiration: exp.UTC(), AutoRenew: si.Renew.Automatic}, nil
}

// SetAutoRenew : renouvellement automatique (ou expiration) chez OVH.
func (o *OVH) SetAutoRenew(ctx context.Context, domain string, on bool) error {
	return o.call(ctx, http.MethodPut, "/domain/"+url.PathEscape(domain)+"/serviceInfos",
		map[string]any{"renew": map[string]any{"automatic": on, "deleteAtExpiration": !on, "forced": false}}, nil)
}

// SetRecords : remplace les enregistrements de même nom (le parking qu'OVH
// pose sur un domaine neuf, un ancien routage), crée les nouveaux, puis
// applique la zone.
func (o *OVH) SetRecords(ctx context.Context, zone string, records []Record) error {
	base := "/domain/zone/" + url.PathEscape(zone)
	for _, r := range records {
		sub := strings.TrimSuffix(strings.TrimSuffix(r.Name, zone), ".")
		if err := o.clearRecords(ctx, base, sub, r.Type); err != nil {
			return err
		}
		in := map[string]any{"fieldType": r.Type, "subDomain": sub, "target": r.Value}
		if r.TTL > 0 {
			in["ttl"] = r.TTL
		}
		if err := o.call(ctx, http.MethodPost, base+"/record", in, nil); err != nil {
			return err
		}
	}
	return o.call(ctx, http.MethodPost, base+"/refresh", nil, nil)
}

// clearRecords : retire ce qui gênerait le nouvel enregistrement : pour un
// routage (A, AAAA, CNAME, ALIAS), tous les routages du même nom ; pour un
// TXT, les TXT du même nom.
func (o *OVH) clearRecords(ctx context.Context, base, sub, typ string) error {
	types := []string{typ}
	if typ != "TXT" {
		types = []string{"A", "AAAA", "CNAME", "ALIAS"}
	}
	for _, t := range types {
		var ids []int64
		q := "?fieldType=" + url.QueryEscape(t) + "&subDomain=" + url.QueryEscape(sub)
		if err := o.call(ctx, http.MethodGet, base+"/record"+q, nil, &ids); err != nil {
			return err
		}
		for _, id := range ids {
			if err := o.call(ctx, http.MethodDelete, base+"/record/"+strconv.FormatInt(id, 10), nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

var _ Registrar = (*OVH)(nil)
