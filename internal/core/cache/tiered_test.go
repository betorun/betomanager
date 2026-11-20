package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memCache : AuthCache en mémoire, exact (pas d'éviction), avec panne
// simulable — joue le Redis partagé ou un cache local.
type memCache struct {
	mu   sync.Mutex
	m    map[string]any
	down bool
}

var errDown = errors.New("redis: connection refused")

func newMem() *memCache { return &memCache{m: map[string]any{}} }

func (c *memCache) put(k string, v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return errDown
	}
	c.m[k] = v
	return nil
}

func (c *memCache) get(k string) (any, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, false, errDown
	}
	v, ok := c.m[k]
	return v, ok, nil
}

func (c *memCache) SetAccessTokenClaims(_ context.Context, t string, v AccessTokenClaims, _ time.Duration) error {
	return c.put("claims:"+t, v)
}
func (c *memCache) GetAccessTokenClaims(_ context.Context, t string) (AccessTokenClaims, bool, error) {
	v, ok, err := c.get("claims:" + t)
	if !ok || err != nil {
		return AccessTokenClaims{}, false, err
	}
	return v.(AccessTokenClaims), true, nil
}
func (c *memCache) BlacklistAccessTokenJTI(_ context.Context, j string, _ time.Duration) error {
	return c.put("jti:"+j, true)
}
func (c *memCache) IsAccessTokenJTIBlacklisted(_ context.Context, j string) (bool, error) {
	_, ok, err := c.get("jti:" + j)
	return ok, err
}
func (c *memCache) SetAPIKeyValidation(_ context.Context, k string, v APIKeyValidation, _ time.Duration) error {
	return c.put("key:"+k, v)
}
func (c *memCache) GetAPIKeyValidation(_ context.Context, k string) (APIKeyValidation, bool, error) {
	v, ok, err := c.get("key:" + k)
	if !ok || err != nil {
		return APIKeyValidation{}, false, err
	}
	return v.(APIKeyValidation), true, nil
}
func (c *memCache) RevokeAPIKeyPrefix(_ context.Context, p string, _ time.Duration) error {
	return c.put("rev:"+p, true)
}
func (c *memCache) IsAPIKeyPrefixRevoked(_ context.Context, p string) (bool, error) {
	_, ok, err := c.get("rev:" + p)
	return ok, err
}
func (c *memCache) Get(_ context.Context, k string) (string, error) {
	v, ok, err := c.get("raw:" + k)
	if !ok || err != nil {
		return "", err
	}
	return v.(string), nil
}
func (c *memCache) Set(_ context.Context, k, v string, _ time.Duration) error {
	return c.put("raw:"+k, v)
}
func (c *memCache) Delete(_ context.Context, k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, "raw:"+k)
	return nil
}

var ctx = context.Background()

// Une déconnexion faite sur une instance, ou avant un redémarrage, vaut
// partout : la révocation est dans le partagé, pas dans la seule mémoire.
func TestTiered_RevocationSharedAcrossInstancesAndRestarts(t *testing.T) {
	shared := newMem()
	a := NewTieredAuthCache(shared, newMem())
	if err := a.BlacklistAccessTokenJTI(ctx, "jti-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeAPIKeyPrefix(ctx, "km_prod_abc", time.Hour); err != nil {
		t.Fatal(err)
	}
	other := NewTieredAuthCache(shared, newMem()) // autre instance, ou même instance redémarrée
	if blocked, err := other.IsAccessTokenJTIBlacklisted(ctx, "jti-1"); err != nil || !blocked {
		t.Fatalf("jeton déconnecté accepté ailleurs : blocked=%v err=%v", blocked, err)
	}
	if revoked, err := other.IsAPIKeyPrefixRevoked(ctx, "km_prod_abc"); err != nil || !revoked {
		t.Fatalf("clé révoquée acceptée ailleurs : revoked=%v err=%v", revoked, err)
	}
}

// Redis en panne : une révocation qu'on ne peut pas vérifier remonte en
// erreur (le middleware refuse), elle ne devient pas « non révoqué ».
func TestTiered_RevocationCheckFailsClosed(t *testing.T) {
	shared := newMem()
	c := NewTieredAuthCache(shared, newMem())
	shared.down = true
	if _, err := c.IsAccessTokenJTIBlacklisted(ctx, "jti-x"); err == nil {
		t.Fatal("panne Redis masquée en « non révoqué » (JTI)")
	}
	if _, err := c.IsAPIKeyPrefixRevoked(ctx, "km_x"); err == nil {
		t.Fatal("panne Redis masquée en « non révoqué » (clé)")
	}
	// Révocation faite pendant la panne : erreur rendue, mais effet local
	// immédiat sur cette instance.
	if err := c.BlacklistAccessTokenJTI(ctx, "jti-2", time.Hour); err == nil {
		t.Fatal("échec d'écriture de la révocation non signalé")
	}
	if blocked, _ := c.IsAccessTokenJTIBlacklisted(ctx, "jti-2"); !blocked {
		t.Fatal("révocation sans effet local pendant la panne")
	}
}

// Les accélérations ne bloquent jamais : panne Redis = absent, l'appelant
// refait la vérification complète.
func TestTiered_AccelerationsDegradeToMiss(t *testing.T) {
	shared := newMem()
	writer := NewTieredAuthCache(shared, newMem())
	claims := AccessTokenClaims{UserID: "u1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := writer.SetAccessTokenClaims(ctx, "tok", claims, time.Minute); err != nil {
		t.Fatal(err)
	}
	reader := NewTieredAuthCache(shared, newMem())
	if got, ok, err := reader.GetAccessTokenClaims(ctx, "tok"); err != nil || !ok || got.UserID != "u1" {
		t.Fatalf("claims partagés non relus : %+v ok=%v err=%v", got, ok, err)
	}
	shared.down = true
	if _, ok, err := reader.GetAccessTokenClaims(ctx, "autre"); ok || err != nil {
		t.Fatalf("panne Redis : ok=%v err=%v, attendu absent sans erreur", ok, err)
	}
	if _, ok, err := reader.GetAPIKeyValidation(ctx, "k"); ok || err != nil {
		t.Fatalf("panne Redis (clé) : ok=%v err=%v", ok, err)
	}
	// Le niveau local sert encore ce qu'il a déjà.
	if _, ok, _ := writer.GetAccessTokenClaims(ctx, "tok"); !ok {
		t.Fatal("niveau local ignoré")
	}
}

func TestTiered_LocalOnlyWithoutShared(t *testing.T) {
	c := NewTieredAuthCache(nil, newMem())
	if err := c.BlacklistAccessTokenJTI(ctx, "j", time.Hour); err != nil {
		t.Fatal(err)
	}
	if blocked, err := c.IsAccessTokenJTIBlacklisted(ctx, "j"); err != nil || !blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
}
