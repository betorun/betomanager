package registrar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOVH_RequiresCredentials(t *testing.T) {
	_, err := NewOVH(OVHConfig{ApplicationKey: "a"})
	assert.Error(t, err)
}

// Signature documentée par OVH : "$1$" + SHA1 des champs joints par "+".
// Valeur attendue calculée indépendamment :
//
//	printf 'as+ck+GET+https://eu.api.ovh.com/1.0/me++1700000000' | sha1sum
func TestOVH_Signature(t *testing.T) {
	o, err := NewOVH(OVHConfig{ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck", Subsidiary: "FR"})
	require.NoError(t, err)
	assert.Equal(t, "$1$"+expectedSHA1, o.sign(http.MethodGet, "https://eu.api.ovh.com/1.0/me", "", 1700000000))
}

func fakeOVH(t *testing.T, offers string) (*OVH, *[]string) {
	t.Helper()
	var calls []string
	var client *OVH
	mux := http.NewServeMux()
	verify := func(r *http.Request, body string) {
		assert.Equal(t, "ak", r.Header.Get("X-Ovh-Application"))
		assert.Equal(t, "ck", r.Header.Get("X-Ovh-Consumer"))
		ts := r.Header.Get("X-Ovh-Timestamp")
		assert.NotEmpty(t, ts)
		assert.Equal(t, client.sign(r.Method, client.cfg.Endpoint+r.URL.RequestURI(), body, client.now().Unix()), r.Header.Get("X-Ovh-Signature"))
	}
	handle := func(pattern string, h func(http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			verify(r, string(raw))
			calls = append(calls, r.Method+" "+r.URL.Path+" "+string(raw))
			h(w, r)
		})
	}
	handle("POST /order/cart", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"cartId":"c1"}`) })
	handle("GET /order/cart/c1/domain", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "exemple.sn", r.URL.Query().Get("domain"))
		_, _ = io.WriteString(w, offers)
	})
	handle("POST /order/cart/c1/domain", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"itemId":7,"prices":[]}`)
	})
	handle("POST /me/contact", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":42}`) })
	handle("POST /order/cart/c1/item/7/configuration", func(http.ResponseWriter, *http.Request) {})
	handle("POST /order/cart/c1/assign", func(http.ResponseWriter, *http.Request) {})
	handle("POST /order/cart/c1/checkout", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"orderId":9001,"prices":{"withTax":{"value":8.49}}}`)
	})
	handle("GET /me/order/9001/status", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `"delivered"`) })
	handle("GET /domain/exemple.sn/serviceInfos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"expiration":"2031-11-19","renew":{"automatic":true}}`)
	})
	handle("PUT /domain/exemple.sn/serviceInfos", func(http.ResponseWriter, *http.Request) {})
	handle("GET /domain/zone/exemple.sn/record", func(w http.ResponseWriter, r *http.Request) {
		// Parking OVH sur la racine : un A à remplacer.
		if r.URL.Query().Get("fieldType") == "A" && r.URL.Query().Get("subDomain") == "" {
			_, _ = io.WriteString(w, `[11]`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})
	handle("DELETE /domain/zone/exemple.sn/record/11", func(http.ResponseWriter, *http.Request) {})
	handle("POST /domain/zone/exemple.sn/record", func(http.ResponseWriter, *http.Request) {})
	handle("POST /domain/zone/exemple.sn/refresh", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var err error
	client, err = NewOVH(OVHConfig{Endpoint: srv.URL, ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck", Subsidiary: "FR"})
	require.NoError(t, err)
	fixed := time.Unix(1700000000, 0)
	client.now = func() time.Time { return fixed }
	return client, &calls
}

func TestOVH_Check(t *testing.T) {
	o, _ := fakeOVH(t, `[{"orderable":true,"action":"create","offer":"gold","prices":[{"label":"RENEW","price":{"value":1,"currencyCode":"EUR"}},{"label":"TOTAL","price":{"value":12.99,"currencyCode":"EUR"}}]}]`)
	a, err := o.Check(context.Background(), "exemple.sn")
	require.NoError(t, err)
	assert.Equal(t, &Availability{Domain: "exemple.sn", Available: true, PriceCents: 1299, Currency: "EUR", RenewCents: 100}, a)

	o, _ = fakeOVH(t, `[{"orderable":false,"action":"create","prices":[]}]`)
	a, err = o.Check(context.Background(), "exemple.sn")
	require.NoError(t, err)
	assert.False(t, a.Available)
}

// Aucune commande réelle ne peut partir tant que le parcours n'est pas validé.
func TestOVH_RegisterDisabled(t *testing.T) {
	o, calls := fakeOVH(t, `[]`)
	_, err := o.Register(context.Background(), Order{Domain: "exemple.sn", Years: 1})
	assert.Error(t, err)
	assert.Empty(t, *calls)
}

func TestOVH_SetRecords(t *testing.T) {
	o, calls := fakeOVH(t, `[]`)
	require.NoError(t, o.SetRecords(context.Background(), "exemple.sn", []Record{
		{Type: "CNAME", Name: "www.exemple.sn", Value: "ingress.betorun.app.", TTL: 300},
		{Type: "A", Name: "exemple.sn", Value: "203.0.113.10"},
	}))
	var posts []string
	deleted := false
	for _, c := range *calls {
		if strings.HasPrefix(c, "POST /domain/zone/exemple.sn/record ") {
			posts = append(posts, c)
		}
		if strings.HasPrefix(c, "DELETE /domain/zone/exemple.sn/record/11") {
			deleted = true
		}
	}
	assert.True(t, deleted, "le parking OVH de la racine est retiré")
	require.Len(t, posts, 2)
	var first map[string]any
	_, raw, _ := cutBody(posts[0])
	require.NoError(t, json.Unmarshal([]byte(raw), &first))
	assert.Equal(t, "www", first["subDomain"])
	assert.EqualValues(t, 300, first["ttl"])
	_, raw, _ = cutBody(posts[1])
	var second map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &second))
	assert.Equal(t, "", second["subDomain"], "racine du domaine")
	assert.Contains(t, (*calls)[len(*calls)-1], "/refresh", "zone appliquée à la fin")
}

func TestOVH_RegisterFlow(t *testing.T) {
	o, calls := fakeOVH(t, `[]`)
	o.cfg.OrderingEnabled = true
	res, err := o.Register(context.Background(), Order{Domain: "exemple.sn", Years: 1, Owner: Contact{FirstName: "Awa", LastName: "Diop",
		Email: "awa@ex.sn", Phone: "+242061234567", Address: "1 rue", City: "Brazzaville", Country: "cg"}})
	require.NoError(t, err)
	assert.Equal(t, "9001", res.OrderID)
	assert.Equal(t, int64(849), res.PriceCents)
	joined := strings.Join(*calls, "\n")
	assert.Contains(t, joined, `"phone":"+242.061234567"`, "format OVH")
	assert.Contains(t, joined, `"value":"/me/contact/42"`, "le client est titulaire")
	assert.Contains(t, joined, `"autoPayWithPreferredPaymentMethod":true`)

	st, err := o.OrderStatus(context.Background(), "9001")
	require.NoError(t, err)
	assert.Equal(t, OrderDelivered, st)
	info, err := o.Info(context.Background(), "exemple.sn")
	require.NoError(t, err)
	assert.Equal(t, 2031, info.Expiration.Year())
	assert.True(t, info.AutoRenew)
	require.NoError(t, o.SetAutoRenew(context.Background(), "exemple.sn", false))
	assert.Contains(t, (*calls)[len(*calls)-1], `"deleteAtExpiration":true`)
}

func cutBody(call string) (string, string, bool) {
	for i := len(call) - 1; i >= 0; i-- {
		if call[i] == '{' && (i == 0 || call[i-1] == ' ') {
			return call[:i], call[i:], true
		}
	}
	return call, "", false
}

// expectedSHA1 : voir TestOVH_Signature.
const expectedSHA1 = "212ae9d571c1dbc085f3630b6c2b68d5b3fdc482"
