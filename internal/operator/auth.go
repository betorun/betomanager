package operator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// Capabilities : domaines ouverts à la personne (calculés par l'API).
type Capabilities struct {
	Infrastructure bool `json:"infrastructure"`
	Support        bool `json:"support"`
	Commercial     bool `json:"commercial"`
	Compute        bool `json:"compute"`
}

// Identity : la personne derrière une requête.
type Identity struct {
	UserID       uuid.UUID    `json:"user_id"`
	Email        string       `json:"email"`
	Capabilities Capabilities `json:"capabilities"`
	// Token : jeton délégué de la personne, pour le chemin normal (API).
	Token string `json:"-"`
	// Rescue : session de secours (compte local) : chemin de secours
	// seulement, jamais les données des clients.
	Rescue bool `json:"rescue"`
}

// Actor : identifiant de la personne dans les dossiers et le journal.
func (id Identity) Actor() string {
	if id.Rescue {
		return "rescue:" + id.UserID.String()
	}
	return id.UserID.String()
}

// Erreurs d'authentification.
var (
	ErrUnauthenticated = errors.New("operator: jeton absent, expiré ou refusé")
	ErrAPIUnavailable  = errors.New("operator: l'API ne répond pas ; utiliser l'accès de secours")
)

// Authenticator : qui est derrière un jeton.
type Authenticator interface {
	Identify(ctx context.Context, token string) (Identity, error)
}

// APIAuth : demande à l'API qui est derrière le jeton délégué présenté
// (GET /admin/operator/me). Réponse gardée une minute : un rôle retiré ou
// une session révoquée cessent d'ouvrir l'Opérateur au plus tard après ce
// délai.
type APIAuth struct {
	BaseURL string // …/kmanager/v1
	HTTP    *http.Client
	TTL     time.Duration
	Now     func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cached
	// group : une seule question à l'API par jeton à la fois (la console
	// ouvre plusieurs requêtes en même temps au chargement d'une page).
	group singleflight.Group
}

type cached struct {
	id  Identity
	exp time.Time
}

const maxCached = 1000

// Identify : Authenticator.
func (a *APIAuth) Identify(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && now.Before(c.exp) {
		a.mu.Unlock()
		c.id.Token = token
		return c.id, nil
	}
	a.mu.Unlock()

	// Partagée entre appelants : détachée de chacun (un appelant parti
	// n'annule pas la réponse des autres), bornée par le délai du client.
	v, err, _ := a.group.Do(string(key[:]), func() (any, error) {
		return a.fetch(context.WithoutCancel(ctx), token)
	})
	if err != nil {
		return Identity{}, err
	}
	id := v.(Identity)
	a.mu.Lock()
	if a.cache == nil || len(a.cache) >= maxCached {
		a.cache = map[[32]byte]cached{}
	}
	ttl := a.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	a.cache[key] = cached{id: id, exp: now.Add(ttl)}
	a.mu.Unlock()
	id.Token = token
	return id, nil
}

func (a *APIAuth) fetch(ctx context.Context, token string) (Identity, error) {
	if a.BaseURL == "" {
		return Identity{}, ErrAPIUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/admin/operator/me", nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := a.HTTP
	if client == nil {
		client = authClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w (%v)", ErrAPIUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Identity{}, ErrUnauthenticated
	case resp.StatusCode != http.StatusOK:
		return Identity{}, fmt.Errorf("%w (HTTP %d)", ErrAPIUnavailable, resp.StatusCode)
	}
	var id Identity
	if err := json.Unmarshal(body, &id); err != nil || id.UserID == uuid.Nil {
		return Identity{}, fmt.Errorf("%w (réponse illisible)", ErrAPIUnavailable)
	}
	return id, nil
}

func (a *APIAuth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// bearer : jeton de l'en-tête Authorization.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

type ctxKey struct{}

// IdentityFrom : la personne de la requête (après authentification).
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}
