// Package ovh : client signé de l'API OVH (https://api.ovh.com), partagé par
// la revente de domaines (registrar) et les VPS (module compute). Aucune
// erreur de ce paquet ne doit atteindre un client de la plateforme (marque
// blanche) : les appelants la journalisent et rendent la leur.
package ovh

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // imposé par la signature de l'API OVH
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Credentials : clé d'application et clé client (secrets, hors du code). La
// clé client doit être limitée aux chemins utiles à son usage.
type Credentials struct {
	Endpoint          string // défaut : https://eu.api.ovh.com/1.0
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
}

const (
	DefaultEndpoint = "https://eu.api.ovh.com/1.0"
	timeout         = 30 * time.Second
	maxResponse     = 4 << 20
	// callsPerSecond, burst : débit plafonné vers l'API (partagé par le process).
	callsPerSecond = 10
	burst          = 20
	// readRetries : reprises d'une lecture refusée pour débit (429) ou
	// indisponibilité (502/503/504) ; maxRetryWait borne le Retry-After.
	readRetries  = 2
	maxRetryWait = 10 * time.Second
)

// ErrIncompleteCredentials : un identifiant manque.
var ErrIncompleteCredentials = errors.New("ovh: incomplete credentials")

// APIError : réponse d'erreur de l'API (statut HTTP et message d'OVH).
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ovh: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound : la ressource n'existe pas (404).
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Client : appels signés.
type Client struct {
	creds   Credentials
	http    *http.Client
	limiter *rate.Limiter
	Now     func() time.Time
}

// transport : connexions gardées ouvertes vers l'API (lectures en parallèle).
func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 16
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// New : client ; erreur si un identifiant manque.
func New(c Credentials) (*Client, error) {
	if c.ApplicationKey == "" || c.ApplicationSecret == "" || c.ConsumerKey == "" {
		return nil, ErrIncompleteCredentials
	}
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	return &Client{creds: c, http: &http.Client{Timeout: timeout, Transport: transport()},
		limiter: rate.NewLimiter(callsPerSecond, burst), Now: time.Now}, nil
}

// Sign : "$1$" + SHA1(secret+clé client+méthode+URL+corps+horodatage).
func (c *Client) Sign(method, fullURL, body string, ts int64) string {
	h := sha1.New() //nolint:gosec
	h.Write([]byte(strings.Join([]string{c.creds.ApplicationSecret, c.creds.ConsumerKey, method, fullURL, body, strconv.FormatInt(ts, 10)}, "+")))
	return "$1$" + hex.EncodeToString(h.Sum(nil))
}

// Call : requête signée ; in (corps JSON) et out (réponse) facultatifs. Une
// lecture refusée pour débit ou indisponibilité est reprise (Retry-After
// respecté, borné) ; une écriture jamais (pas de double commande).
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	for attempt := 0; ; attempt++ {
		wait, err := c.call(ctx, method, path, in, out)
		if wait == 0 || method != http.MethodGet || attempt >= readRetries {
			return err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// retryable : statuts repris pour une lecture.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter : délai annoncé (secondes), 1 s par défaut, borné.
func retryAfter(h string) time.Duration {
	d := time.Second
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	return min(d, maxRetryWait)
}

// call : un essai ; wait > 0 si l'essai peut être repris après ce délai.
func (c *Client) call(ctx context.Context, method, path string, in, out any) (time.Duration, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, err
		}
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return 0, err
		}
	}
	full := c.creds.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := c.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ovh-Application", c.creds.ApplicationKey)
	req.Header.Set("X-Ovh-Consumer", c.creds.ConsumerKey)
	req.Header.Set("X-Ovh-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Ovh-Signature", c.Sign(method, full, string(body), ts))
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		apiErr := &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: e.Message}
		if retryable(resp.StatusCode) {
			return retryAfter(resp.Header.Get("Retry-After")), apiErr
		}
		return 0, apiErr
	}
	if out == nil || len(raw) == 0 {
		return 0, nil
	}
	return 0, json.Unmarshal(raw, out)
}
