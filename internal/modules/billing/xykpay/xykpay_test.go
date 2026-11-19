package xykpay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"type":"payment.succeeded","data":{"object":{"id":"p1"}}}`)
	sig := Sign("s3cret", now.Unix(), body)
	ok := fmt.Sprintf("t=%d,v1=%s", now.Unix(), sig)

	assert.NoError(t, Verify("s3cret", ok, body, now))
	assert.ErrorIs(t, Verify("other", ok, body, now), ErrBadSignature)
	assert.ErrorIs(t, Verify("s3cret", ok, []byte(`{"tampered":true}`), now), ErrBadSignature)
	assert.ErrorIs(t, Verify("s3cret", ok, body, now.Add(10*time.Minute)), ErrBadSignature, "trop vieille")
	assert.ErrorIs(t, Verify("", ok, body, now), ErrBadSignature)
	assert.NoError(t, Verify("s3cret", fmt.Sprintf("t=%d,v1=bad,v1=%s", now.Unix(), sig), body, now), "rotation : une v1 suffit")

	assert.Equal(t, "p1", PaymentID(body))
	assert.Empty(t, PaymentID([]byte(`{"type":"payout.paid","data":{"object":{"id":"x"}}}`)))
}

func TestClient(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer key", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/payments":
			assert.Equal(t, "idem", r.Header.Get("Idempotency-Key"))
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"id":"p1","status":"pending","environment":"sandbox"}`))
		case r.URL.Path == "/api/v1/payments/p1":
			_, _ = w.Write([]byte(`{"id":"p1","status":"succeeded"}`))
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"MOBILE_MONEY_INSUFFICIENT_FUNDS","message":"no"}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "key")

	p, err := c.Create(context.Background(), Charge{Amount: 5000, Currency: "XAF", Phone: "+242061234567", Operator: "cg_mtn"}, "idem")
	require.NoError(t, err)
	assert.Equal(t, "pending", p.Status)
	assert.Equal(t, "mobile_money", got["method"])
	assert.EqualValues(t, 5000, got["amount"])

	p, err = c.Get(context.Background(), "p1")
	require.NoError(t, err)
	assert.Equal(t, "succeeded", p.Status)

	_, err = c.Get(context.Background(), "missing")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "MOBILE_MONEY_INSUFFICIENT_FUNDS", apiErr.Code)
}
