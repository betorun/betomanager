package cache

import (
	"context"
	"time"
)

// TieredAuthCache : cache d'authentification à deux niveaux, partagé
// (Redis : commun aux instances de l'API, survit à un redémarrage) et local
// (ristretto : mémoire du process, sans aller-retour réseau).
//
// Deux règles différentes selon ce qui est rangé :
//
//   - Accélérations (claims d'un jeton vérifié, validation d'une clé) : local
//     d'abord, puis partagé. Une panne du partagé vaut « absent » : l'appelant
//     refait la vérification complète, rien n'est accordé à tort.
//   - Révocations (JTI d'un jeton déconnecté, préfixe d'une clé révoquée) :
//     écrites dans les deux ; lues en local puis dans le partagé, et une panne
//     du partagé REMONTE en erreur. Retomber en silence sur le seul local
//     dirait « pas révoqué » pour une révocation faite ailleurs ou avant un
//     redémarrage : le middleware refuse au contraire quand il ne peut pas
//     vérifier (refuseRevokedJTI).
type TieredAuthCache struct {
	shared AuthCache
	local  AuthCache
}

// NewTieredAuthCache : shared peut être nil (local seul, tests et outils).
func NewTieredAuthCache(shared, local AuthCache) *TieredAuthCache {
	return &TieredAuthCache{shared: shared, local: local}
}

// Close ferme le niveau local (le client Redis appartient à l'application).
func (c *TieredAuthCache) Close() {
	if l, ok := c.local.(interface{ Close() }); ok {
		l.Close()
	}
}

// ── Accélérations ───────────────────────────────────────────────────────

func (c *TieredAuthCache) SetAccessTokenClaims(ctx context.Context, token string, claims AccessTokenClaims, ttl time.Duration) error {
	_ = c.local.SetAccessTokenClaims(ctx, token, claims, ttl)
	if c.shared == nil {
		return nil
	}
	return c.shared.SetAccessTokenClaims(ctx, token, claims, ttl)
}

func (c *TieredAuthCache) GetAccessTokenClaims(ctx context.Context, token string) (AccessTokenClaims, bool, error) {
	if v, ok, err := c.local.GetAccessTokenClaims(ctx, token); err == nil && ok {
		return v, true, nil
	}
	if c.shared == nil {
		return AccessTokenClaims{}, false, nil
	}
	v, ok, err := c.shared.GetAccessTokenClaims(ctx, token)
	if err != nil || !ok {
		return AccessTokenClaims{}, false, nil // panne : vérification complète
	}
	return v, true, nil
}

func (c *TieredAuthCache) SetAPIKeyValidation(ctx context.Context, rawKey string, validation APIKeyValidation, ttl time.Duration) error {
	_ = c.local.SetAPIKeyValidation(ctx, rawKey, validation, ttl)
	if c.shared == nil {
		return nil
	}
	return c.shared.SetAPIKeyValidation(ctx, rawKey, validation, ttl)
}

func (c *TieredAuthCache) GetAPIKeyValidation(ctx context.Context, rawKey string) (APIKeyValidation, bool, error) {
	if v, ok, err := c.local.GetAPIKeyValidation(ctx, rawKey); err == nil && ok {
		return v, true, nil
	}
	if c.shared == nil {
		return APIKeyValidation{}, false, nil
	}
	v, ok, err := c.shared.GetAPIKeyValidation(ctx, rawKey)
	if err != nil || !ok {
		return APIKeyValidation{}, false, nil // panne : validation en base
	}
	return v, true, nil
}

// ── Révocations ─────────────────────────────────────────────────────────

func (c *TieredAuthCache) BlacklistAccessTokenJTI(ctx context.Context, jti string, ttl time.Duration) error {
	_ = c.local.BlacklistAccessTokenJTI(ctx, jti, ttl) // effet immédiat ici, même Redis en panne
	if c.shared == nil {
		return nil
	}
	return c.shared.BlacklistAccessTokenJTI(ctx, jti, ttl)
}

func (c *TieredAuthCache) IsAccessTokenJTIBlacklisted(ctx context.Context, jti string) (bool, error) {
	if blocked, err := c.local.IsAccessTokenJTIBlacklisted(ctx, jti); err == nil && blocked {
		return true, nil
	}
	if c.shared == nil {
		return false, nil
	}
	return c.shared.IsAccessTokenJTIBlacklisted(ctx, jti)
}

func (c *TieredAuthCache) RevokeAPIKeyPrefix(ctx context.Context, prefix string, ttl time.Duration) error {
	_ = c.local.RevokeAPIKeyPrefix(ctx, prefix, ttl)
	if c.shared == nil {
		return nil
	}
	return c.shared.RevokeAPIKeyPrefix(ctx, prefix, ttl)
}

func (c *TieredAuthCache) IsAPIKeyPrefixRevoked(ctx context.Context, prefix string) (bool, error) {
	if revoked, err := c.local.IsAPIKeyPrefixRevoked(ctx, prefix); err == nil && revoked {
		return true, nil
	}
	if c.shared == nil {
		return false, nil
	}
	return c.shared.IsAPIKeyPrefixRevoked(ctx, prefix)
}

// ── Valeurs génériques : le partagé fait foi ────────────────────────────

func (c *TieredAuthCache) Get(ctx context.Context, key string) (string, error) {
	if c.shared == nil {
		return c.local.Get(ctx, key)
	}
	return c.shared.Get(ctx, key)
}

func (c *TieredAuthCache) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	if c.shared == nil {
		return c.local.Set(ctx, key, value, ttl)
	}
	return c.shared.Set(ctx, key, value, ttl)
}

func (c *TieredAuthCache) Delete(ctx context.Context, key string) error {
	_ = c.local.Delete(ctx, key)
	if c.shared == nil {
		return nil
	}
	return c.shared.Delete(ctx, key)
}
