package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLedger : plafond de dépense par organisation et par jour (UTC), dans
// Redis, partagé par toutes les instances de l'API (même choix que le rate
// limit, docs/adr/0006). Fail-closed : Redis injoignable = refus.
type RedisLedger struct {
	rdb   redis.UniversalClient
	limit int64 // micro-dollars par organisation et par jour
	now   func() time.Time
}

// NewRedisLedger : dailyLimitMicroUSD > 0.
func NewRedisLedger(rdb redis.UniversalClient, dailyLimitMicroUSD int64) (*RedisLedger, error) {
	if rdb == nil {
		return nil, fmt.Errorf("llm: ledger sans Redis")
	}
	if dailyLimitMicroUSD <= 0 {
		return nil, fmt.Errorf("llm: plafond journalier invalide (%d)", dailyLimitMicroUSD)
	}
	return &RedisLedger{rdb: rdb, limit: dailyLimitMicroUSD, now: time.Now}, nil
}

// reserveScript : ajoute ARGV[1] au compteur si le plafond ARGV[2] n'est pas
// dépassé ; rend le nouveau total, ou -1 (refus). Atomique.
var reserveScript = redis.NewScript(`
local cur = tonumber(redis.call("GET", KEYS[1]) or "0")
local amt = tonumber(ARGV[1])
if cur + amt > tonumber(ARGV[2]) then
  return -1
end
local v = redis.call("INCRBY", KEYS[1], amt)
redis.call("EXPIRE", KEYS[1], ARGV[3])
return v
`)

// settleScript : applique l'écart (négatif = remboursement), sans descendre
// sous zéro.
var settleScript = redis.NewScript(`
local v = redis.call("INCRBY", KEYS[1], ARGV[1])
if v < 0 then
  redis.call("SET", KEYS[1], 0, "KEEPTTL")
  v = 0
end
return v
`)

const ledgerTTLSeconds = 2 * 24 * 3600

func (l *RedisLedger) key(orgID string, day time.Time) string {
	return "betorun:llm:spend:" + orgID + ":" + day.UTC().Format("20060102")
}

func (l *RedisLedger) Reserve(ctx context.Context, orgID string, microUSD int64) (Reservation, error) {
	if microUSD < 0 {
		microUSD = 0
	}
	key := l.key(orgID, l.now())
	v, err := reserveScript.Run(ctx, l.rdb, []string{key}, microUSD, l.limit, ledgerTTLSeconds).Int64()
	if err != nil {
		return nil, &Error{Kind: KindBudgetUnavailable, Err: err}
	}
	if v < 0 {
		return nil, &Error{Kind: KindBudgetExceeded, Message: "plafond journalier d'IA de l'organisation atteint"}
	}
	return &redisReservation{l: l, key: key, reserved: microUSD}, nil
}

type redisReservation struct {
	l        *RedisLedger
	key      string // la clé du jour de la RÉSERVATION (un appel à cheval sur minuit reste compté sur son jour)
	reserved int64
	once     sync.Once
}

func (r *redisReservation) Settle(ctx context.Context, actual int64) error {
	var err error
	r.once.Do(func() {
		if actual < 0 {
			actual = 0
		}
		if delta := actual - r.reserved; delta != 0 {
			err = settleScript.Run(ctx, r.l.rdb, []string{r.key}, delta).Err()
		}
	})
	return err
}

// MemoryLedger : registre en mémoire, pour les tests et le développement local
// sans Redis. Plafond par organisation, sans notion de jour.
type MemoryLedger struct {
	mu    sync.Mutex
	limit int64
	spent map[string]int64
}

func NewMemoryLedger(limitMicroUSD int64) *MemoryLedger {
	return &MemoryLedger{limit: limitMicroUSD, spent: map[string]int64{}}
}

func (m *MemoryLedger) Spent(orgID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent[orgID]
}

func (m *MemoryLedger) Reserve(_ context.Context, orgID string, microUSD int64) (Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spent[orgID]+microUSD > m.limit {
		return nil, &Error{Kind: KindBudgetExceeded, Message: "plafond d'IA de l'organisation atteint"}
	}
	m.spent[orgID] += microUSD
	return &memReservation{m: m, org: orgID, reserved: microUSD}, nil
}

type memReservation struct {
	m        *MemoryLedger
	org      string
	reserved int64
	once     sync.Once
}

func (r *memReservation) Settle(_ context.Context, actual int64) error {
	r.once.Do(func() {
		if actual < 0 {
			actual = 0
		}
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		r.m.spent[r.org] += actual - r.reserved
		if r.m.spent[r.org] < 0 {
			r.m.spent[r.org] = 0
		}
	})
	return nil
}
