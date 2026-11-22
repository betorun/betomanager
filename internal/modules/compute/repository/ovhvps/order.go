package ovhvps

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

var _ service.Orderer = (*Provider)(nil)

// Libellés de configuration d'un VPS dans le panier.
const (
	cfgDatacenter = "vps_datacenter"
	cfgOS         = "vps_os"
)

type cartItem struct {
	ItemID int64 `json:"itemId"`
}

type requirement struct {
	Label    string   `json:"label"`
	Required bool     `json:"required"`
	Fields   []string `json:"fields"`
}

// Order : panier → VPS (1 mois) → datacenter et système → rattachement →
// contrôle du prix → paiement par le moyen par défaut du compte.
func (p *Provider) Order(ctx context.Context, spec service.OrderSpec) (string, error) {
	var cart struct {
		CartID string `json:"cartId"`
	}
	if err := p.api.Call(ctx, http.MethodPost, "/order/cart", map[string]any{
		"ovhSubsidiary": p.subsidiary, "description": spec.Reference,
	}, &cart); err != nil {
		return "", err
	}
	base := "/order/cart/" + url.PathEscape(cart.CartID)
	var item cartItem
	if err := p.api.Call(ctx, http.MethodPost, base+"/vps", map[string]any{
		"planCode": spec.PlanCode, "duration": "P1M", "pricingMode": "default", "quantity": 1,
	}, &item); err != nil {
		return "", err
	}
	itemPath := base + "/item/" + strconv.FormatInt(item.ItemID, 10)
	var reqs []requirement
	if err := p.api.Call(ctx, http.MethodGet, itemPath+"/requiredConfiguration", nil, &reqs); err != nil {
		return "", err
	}
	values := map[string]string{}
	for _, r := range reqs {
		switch r.Label {
		case cfgDatacenter:
			v, ok := pickDatacenter(r.Fields, spec.Location)
			if !ok {
				return "", fmt.Errorf("compute: location %q not offered for plan %s", spec.Location, spec.PlanCode)
			}
			values[r.Label] = v
		case cfgOS:
			v, ok := pickOS(r.Fields, spec.Image)
			if !ok {
				return "", fmt.Errorf("%w: %q", ErrImageNotFound, spec.Image)
			}
			values[r.Label] = v
		default:
			if r.Required {
				return "", fmt.Errorf("compute: unexpected required configuration %q", r.Label)
			}
		}
	}
	for _, label := range []string{cfgDatacenter, cfgOS} {
		if v, ok := values[label]; ok {
			if err := p.api.Call(ctx, http.MethodPost, itemPath+"/configuration", map[string]any{"label": label, "value": v}, nil); err != nil {
				return "", err
			}
		}
	}
	if err := p.api.Call(ctx, http.MethodPost, base+"/assign", nil, nil); err != nil {
		return "", err
	}
	// Aperçu : le prix du fournisseur ne dépasse pas celui de l'offre.
	var preview struct {
		Prices struct {
			WithTax struct {
				Value float64 `json:"value"`
			} `json:"withTax"`
		} `json:"prices"`
	}
	if err := p.api.Call(ctx, http.MethodGet, base+"/checkout", nil, &preview); err != nil {
		return "", err
	}
	if cents := int64(math.Round(preview.Prices.WithTax.Value * 100)); spec.MaxPriceCents > 0 && cents > spec.MaxPriceCents {
		return "", service.ErrProviderPriceAbove
	}
	var order struct {
		OrderID int64 `json:"orderId"`
	}
	if err := p.api.Call(ctx, http.MethodPost, base+"/checkout", map[string]any{
		"autoPayWithPreferredPaymentMethod": true, "waiveRetractationPeriod": true,
	}, &order); err != nil {
		return "", err
	}
	return strconv.FormatInt(order.OrderID, 10), nil
}

// pickDatacenter : la valeur du fournisseur qui correspond au code de la
// plateforme (« gra » → « GRA » ou « GRA7 »…).
func pickDatacenter(fields []string, location string) (string, bool) {
	loc := strings.ToLower(location)
	for _, f := range fields {
		if strings.ToLower(f) == loc {
			return f, true
		}
	}
	for _, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), loc) {
			return f, true
		}
	}
	return "", false
}

// pickOS : la valeur du fournisseur pour ce système (nom réglé dans l'offre).
func pickOS(fields []string, image string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(image))
	for _, f := range fields {
		if strings.ToLower(strings.TrimSpace(f)) == want {
			return f, true
		}
	}
	return "", false
}

// OrderState : suivi de la commande ; livrée, l'identifiant du VPS est lu
// dans le détail de la commande.
func (p *Provider) OrderState(ctx context.Context, orderID string) (string, string, error) {
	if _, err := strconv.ParseInt(orderID, 10, 64); err != nil {
		return "", "", fmt.Errorf("compute: invalid order id %q", orderID)
	}
	base := "/me/order/" + orderID
	var status string
	if err := p.api.Call(ctx, http.MethodGet, base+"/status", nil, &status); err != nil {
		return "", "", err
	}
	switch status {
	case "cancelled", "cancelling", "notPaid":
		return service.OrderFailed, "", nil
	case "delivered":
	default: // checking, delivering, documentsRequested, unknown
		return service.OrderPending, "", nil
	}
	var ids []int64
	if err := p.api.Call(ctx, http.MethodGet, base+"/details", nil, &ids); err != nil {
		return "", "", err
	}
	for _, id := range ids {
		var d struct {
			Domain string `json:"domain"`
		}
		if err := p.api.Call(ctx, http.MethodGet, base+"/details/"+strconv.FormatInt(id, 10), nil, &d); err != nil {
			return "", "", err
		}
		if strings.HasPrefix(d.Domain, "vps-") || strings.Contains(d.Domain, ".vps.") {
			return service.OrderDelivered, d.Domain, nil
		}
	}
	// Livrée mais le VPS n'est pas encore rattaché à la commande.
	return service.OrderPending, "", nil
}
