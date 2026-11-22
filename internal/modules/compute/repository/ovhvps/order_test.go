package ovhvps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type recorded struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func (r *recorded) get(key string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[key]
}

func fakeOrder(t *testing.T, routes map[string]string) (*Provider, *recorded) {
	t.Helper()
	rec := &recorded{bodies: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rec.mu.Lock()
		rec.bodies[key] = append(rec.bodies[key], body)
		rec.mu.Unlock()
		out, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
			return
		}
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	p, err := New(ovh.Credentials{Endpoint: srv.URL, ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck"}, "FR")
	require.NoError(t, err)
	return p, rec
}

func orderRoutes(priceWithTax string) map[string]string {
	return map[string]string{
		"POST /order/cart":                                `{"cartId":"c1"}`,
		"POST /order/cart/c1/vps":                         `{"itemId":7}`,
		"GET /order/cart/c1/item/7/requiredConfiguration": `[{"label":"vps_datacenter","required":true,"fields":["BHS","GRA","SBG"]},{"label":"vps_os","required":true,"fields":["Debian 12","Ubuntu 24.04"]}]`,
		"POST /order/cart/c1/item/7/configuration":        `{"id":1}`,
		"POST /order/cart/c1/assign":                      ``,
		"GET /order/cart/c1/checkout":                     `{"prices":{"withTax":{"value":` + priceWithTax + `}}}`,
		"POST /order/cart/c1/checkout":                    `{"orderId":9001}`,
	}
}

var spec = service.OrderSpec{Reference: "vps-uuid", PlanCode: "vps-2025-model1", Location: "gra", Image: "Ubuntu 24.04", MaxPriceCents: 649}

func TestOrder(t *testing.T) {
	p, rec := fakeOrder(t, orderRoutes("6.49"))
	id, err := p.Order(context.Background(), spec)
	require.NoError(t, err)
	assert.Equal(t, "9001", id)

	assert.Equal(t, "FR", rec.get("POST /order/cart")[0]["ovhSubsidiary"])
	item := rec.get("POST /order/cart/c1/vps")[0]
	assert.Equal(t, "vps-2025-model1", item["planCode"])
	assert.Equal(t, "P1M", item["duration"], "un mois")
	cfg := rec.get("POST /order/cart/c1/item/7/configuration")
	require.Len(t, cfg, 2)
	assert.Equal(t, map[string]any{"label": "vps_datacenter", "value": "GRA"}, cfg[0])
	assert.Equal(t, map[string]any{"label": "vps_os", "value": "Ubuntu 24.04"}, cfg[1])
	assert.Equal(t, true, rec.get("POST /order/cart/c1/checkout")[0]["autoPayWithPreferredPaymentMethod"])
}

func TestOrder_PriceAboveNotPaid(t *testing.T) {
	p, rec := fakeOrder(t, orderRoutes("7.20"))
	_, err := p.Order(context.Background(), spec)
	assert.ErrorIs(t, err, service.ErrProviderPriceAbove)
	assert.Empty(t, rec.get("POST /order/cart/c1/checkout"), "rien payé")
}

func TestOrder_LocationOrImageNotOffered(t *testing.T) {
	p, rec := fakeOrder(t, orderRoutes("6.49"))
	s := spec
	s.Location = "rbx"
	_, err := p.Order(context.Background(), s)
	assert.Error(t, err)
	s = spec
	s.Image = "Windows"
	_, err = p.Order(context.Background(), s)
	assert.ErrorIs(t, err, ErrImageNotFound)
	assert.Empty(t, rec.get("POST /order/cart/c1/checkout"))
}

func TestOrderState(t *testing.T) {
	p, _ := fakeOrder(t, map[string]string{
		"GET /me/order/9001/status":    `"delivered"`,
		"GET /me/order/9001/details":   `[1,2]`,
		"GET /me/order/9001/details/1": `{"domain":"*001"}`,
		"GET /me/order/9001/details/2": `{"domain":"vps-0a1b2c3d.vps.ovh.net"}`,
		"GET /me/order/9002/status":    `"delivering"`,
		"GET /me/order/9003/status":    `"notPaid"`,
	})
	st, sn, err := p.OrderState(context.Background(), "9001")
	require.NoError(t, err)
	assert.Equal(t, service.OrderDelivered, st)
	assert.Equal(t, "vps-0a1b2c3d.vps.ovh.net", sn)
	st, _, _ = p.OrderState(context.Background(), "9002")
	assert.Equal(t, service.OrderPending, st)
	st, _, _ = p.OrderState(context.Background(), "9003")
	assert.Equal(t, service.OrderFailed, st)
	_, _, err = p.OrderState(context.Background(), "../me")
	assert.Error(t, err)
}

func TestRebuildAndPower(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{
		"GET /vps/vps-a/images/available":    `["i1","i2"]`,
		"GET /vps/vps-a/images/available/i1": `{"id":"i1","name":"Debian 12"}`,
		"GET /vps/vps-a/images/available/i2": `{"id":"i2","name":"Ubuntu 24.04"}`,
		"POST /vps/vps-a/rebuild":            `{"id":55,"state":"todo"}`,
		"POST /vps/vps-a/stop":               `{"id":56}`,
	})
	task, err := p.Rebuild(context.Background(), "vps-a", "ubuntu 24.04", "ssh-ed25519 AAAA")
	require.NoError(t, err)
	assert.Equal(t, "55", task)
	assert.Equal(t, map[string]any{"imageId": "i2", "publicSshKey": "ssh-ed25519 AAAA", "doNotSendPassword": true},
		rec.get("POST /vps/vps-a/rebuild")[0], "clé du client, aucun mot de passe envoyé")
	task, err = p.Stop(context.Background(), "vps-a")
	require.NoError(t, err)
	assert.Equal(t, "56", task)
	_, err = p.Rebuild(context.Background(), "vps-a", "Windows", "k")
	assert.ErrorIs(t, err, ErrImageNotFound)
}
