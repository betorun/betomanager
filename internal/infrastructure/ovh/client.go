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
	creds Credentials
	http  *http.Client
	Now   func() time.Time
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
	return &Client{creds: c, http: &http.Client{Timeout: timeout}, Now: time.Now}, nil
}

// Sign : "$1$" + SHA1(secret+clé client+méthode+URL+corps+horodatage).
func (c *Client) Sign(method, fullURL, body string, ts int64) string {
	h := sha1.New() //nolint:gosec
	h.Write([]byte(strings.Join([]string{c.creds.ApplicationSecret, c.creds.ConsumerKey, method, fullURL, body, strconv.FormatInt(ts, 10)}, "+")))
	return "$1$" + hex.EncodeToString(h.Sum(nil))
}

// Call : requête signée ; in (corps JSON) et out (réponse) facultatifs.
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	full := c.creds.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := c.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ovh-Application", c.creds.ApplicationKey)
	req.Header.Set("X-Ovh-Consumer", c.creds.ConsumerKey)
	req.Header.Set("X-Ovh-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Ovh-Signature", c.Sign(method, full, string(body), ts))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
