// Package repository : cache Redis des constats.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache : résultats des constats dans Redis (clé par projet et droits).
type RedisCache struct{ Client redis.UniversalClient }

// Get : valeur gardée ; ok faux si absente ou expirée.
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	raw, err := c.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// Set : garde value pendant ttl.
func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.Client.Set(ctx, key, value, ttl).Err()
}
