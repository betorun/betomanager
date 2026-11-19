// Package xykpay : rail de paiement de la facturation, par l'API publique de
// XYKPay, comme n'importe quel marchand (clé secrète du compte betorun,
// Idempotency-Key). XYKPay ne fait que transporter l'argent : le solde et le
// grand livre restent dans betorun (ADR 0012, décision 2).
//
// Le webhook ne sert que de signal : il ne porte que l'identifiant du
// paiement, relu ensuite par l'API (on ne croit jamais le corps d'un webhook).
package xykpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Payment : état d'un paiement chez XYKPay.
type Payment struct {
	ID          string `json:"id"`
	Status      string `json:"status"` // created, pending, succeeded, failed, cancelled
	FailureCode string `json:"failure_code"`
	Environment string `json:"environment"` // sandbox ou live
}

// Charge : demande poussée sur le téléphone du client.
type Charge struct {
	Amount       int64 // unités de la monnaie
	Currency     string
	Phone        string
	Operator     string
	CustomerName string
	Description  string
	Reference    string
}

// APIError : refus de l'API XYKPay.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("xykpay %d %s: %s", e.Status, e.Code, e.Message)
}

// Client : API de paiements de XYKPay.
type Client struct {
	BaseURL string // ex. https://pay.sykedu.com (sans /api/v1)
	APIKey  string
	HTTP    *http.Client
}

// New : client prêt à l'emploi.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Create : paiement mobile money ; key rend la création idempotente.
func (c *Client) Create(ctx context.Context, ch Charge, key string) (*Payment, error) {
	body := map[string]any{"amount": ch.Amount, "currency": ch.Currency, "method": "mobile_money", "phone": ch.Phone,
		"operator": ch.Operator, "customer_name": ch.CustomerName, "description": ch.Description, "reference": ch.Reference}
	return c.do(ctx, http.MethodPost, "/payments", body, key)
}

// Get : relit un paiement.
func (c *Client) Get(ctx context.Context, id string) (*Payment, error) {
	return c.do(ctx, http.MethodGet, "/payments/"+url.PathEscape(id), nil, "")
}

func (c *Client) do(ctx context.Context, method, path string, body any, key string) (*Payment, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var e struct{ Error, Code, Message string }
		_ = json.Unmarshal(raw, &e)
		code := e.Code
		if code == "" {
			code = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Code: code, Message: e.Message}
	}
	var p Payment
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" {
		return nil, errors.New("xykpay: unreadable payment")
	}
	return &p, nil
}

// ── Webhook ─────────────────────────────────────────────────────────────

// SignatureHeader : en-tête de signature des webhooks.
const SignatureHeader = "XYK-Signature"

// Tolerance : âge maximal d'une signature.
const Tolerance = 5 * time.Minute

var ErrBadSignature = errors.New("invalid webhook signature")

// Verify : "t=<unix>,v1=<hex hmac-sha256(secret, t.body)>" (au moins une v1
// valide), horodatage récent.
func Verify(secret, header string, body []byte, now time.Time) error {
	if secret == "" {
		return ErrBadSignature
	}
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrBadSignature
	}
	want := Sign(secret, ts, body)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return ErrBadSignature
}

// Sign : signature v1 (tests, et outils de rejeu).
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Event : événement de paiement (seul l'identifiant sert).
type Event struct {
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	} `json:"data"`
}

// PaymentID : paiement concerné par un événement de paiement ; "" sinon.
func PaymentID(body []byte) string {
	var ev Event
	if json.Unmarshal(body, &ev) != nil || !strings.HasPrefix(ev.Type, "payment.") {
		return ""
	}
	return ev.Data.Object.ID
}
