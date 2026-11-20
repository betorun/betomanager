package cache

// Contre un vrai Redis (ou Valkey), jetable :
//
//	redis-server --port 6390 --save '' --appendonly no &
//	AUTHCACHE_REDIS_ADDR=127.0.0.1:6390 go test ./internal/core/cache/ -run TestE2E -v

import (
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func e2eRedis(t *testing.T) *redis.Client {
	addr := os.Getenv("AUTHCACHE_REDIS_ADDR")
	if addr == "" {
		t.Skip("AUTHCACHE_REDIS_ADDR not set")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestE2E_RedisAuthCache(t *testing.T) {
	client := e2eRedis(t)
	r := NewRedisAuthCacheFromClient(client)
	suffix := time.Now().Format("150405.000000000")

	// Absent : ni erreur ni valeur (redis.Nil).
	if _, ok, err := r.GetAccessTokenClaims(ctx, "absent-"+suffix); ok || err != nil {
		t.Fatalf("absent : ok=%v err=%v", ok, err)
	}
	claims := AccessTokenClaims{UserID: "u1", Roles: []string{"owner"}, ExpiresAt: 42, JTI: "j"}
	if err := r.SetAccessTokenClaims(ctx, "tok-"+suffix, claims, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.GetAccessTokenClaims(ctx, "tok-"+suffix)
	if err != nil || !ok || got.UserID != "u1" || len(got.Roles) != 1 {
		t.Fatalf("claims : %+v ok=%v err=%v", got, ok, err)
	}

	// Révocation partagée entre deux caches à deux niveaux (deux instances).
	a := NewTieredAuthCache(r, newMem())
	b := NewTieredAuthCache(NewRedisAuthCacheFromClient(client), newMem())
	if err := a.BlacklistAccessTokenJTI(ctx, "jti-"+suffix, time.Minute); err != nil {
		t.Fatal(err)
	}
	if blocked, err := b.IsAccessTokenJTIBlacklisted(ctx, "jti-"+suffix); err != nil || !blocked {
		t.Fatalf("révocation non vue par l'autre instance : %v %v", blocked, err)
	}

	// TTL respecté.
	if err := r.RevokeAPIKeyPrefix(ctx, "pfx-"+suffix, 1100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if revoked, _ := r.IsAPIKeyPrefixRevoked(ctx, "pfx-"+suffix); !revoked {
		t.Fatal("révocation de clé non écrite")
	}
	time.Sleep(1500 * time.Millisecond)
	if revoked, _ := r.IsAPIKeyPrefixRevoked(ctx, "pfx-"+suffix); revoked {
		t.Fatal("TTL ignoré")
	}

	// Les clés sont hachées : ni jeton ni JTI en clair dans Redis.
	keys, err := client.Keys(ctx, "auth:*"+suffix+"*").Result()
	if err != nil || len(keys) != 0 {
		t.Fatalf("valeur en clair dans une clé Redis : %v %v", keys, err)
	}

	// Serveur injoignable : erreur, et le cache à deux niveaux refuse.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })
	tiered := NewTieredAuthCache(NewRedisAuthCacheFromClient(down), newMem())
	if _, err := tiered.IsAccessTokenJTIBlacklisted(ctx, "x"); err == nil {
		t.Fatal("Redis injoignable : révocation non vérifiée mais acceptée")
	}
}
