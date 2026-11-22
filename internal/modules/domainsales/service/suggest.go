package service

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/domains/registrar"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
)

// Suggestions de noms (docs/modules/domainsales.md) : le nom recherché sur
// d'autres extensions, puis des variantes, chacun vérifié chez le registraire
// et tarifé comme une recherche. Les réponses sont gardées quelques minutes :
// une recherche tapée plusieurs fois ne refait pas les appels.

const (
	// suggestBudget : au-delà, les noms non vérifiés sont rendus « unknown ».
	suggestBudget = 8 * time.Second
	// suggestCacheTTL : durée de vie d'une disponibilité en cache (jamais
	// utilisée pour un achat : Buy revérifie).
	suggestCacheTTL = 10 * time.Minute
	suggestCacheMax = 5000
	// fallbackConcurrency : vérifications simultanées sans BatchChecker.
	fallbackConcurrency = 4
)

type cachedAvailability struct {
	a       *registrar.Availability
	expires time.Time
}

// availabilityCache : disponibilités récentes, partagées entre organisations
// (elles ne dépendent que du nom).
type availabilityCache struct {
	mu   sync.Mutex
	rows map[string]cachedAvailability
}

func (c *availabilityCache) get(name string, now time.Time) (*registrar.Availability, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rows[name]
	if !ok || now.After(r.expires) {
		return nil, false
	}
	return r.a, true
}

func (c *availabilityCache) put(name string, a *registrar.Availability, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil || len(c.rows) >= suggestCacheMax {
		c.rows = map[string]cachedAvailability{} // borne mémoire : purge simple
	}
	c.rows[name] = cachedAvailability{a: a, expires: now.Add(suggestCacheTTL)}
}

// Suggest : suggestions pour un nom ou un mot-clé. Les extensions que le
// registraire ne vend pas sont omises ; un nom non vérifié à temps est rendu
// avec la raison « unknown » (jamais d'erreur du fournisseur).
func (s *Service) Suggest(ctx context.Context, org uuid.UUID, raw string) (*domain.SuggestionsResponse, error) {
	if s.Registrar == nil {
		return nil, domain.ErrSalesDisabled
	}
	label, tld, ok := domain.SplitQuery(raw, "com")
	if !ok {
		return nil, domain.ErrInvalidDomain
	}
	c, err := s.Wallet.AccountCatalog(ctx, org)
	if err != nil {
		return nil, err
	}
	tlds := s.SuggestTLDs
	if len(tlds) == 0 {
		tlds = domain.DefaultSuggestTLDs
	}
	candidates := domain.SuggestCandidates(label, tld, tlds)
	out := &domain.SuggestionsResponse{Query: label + "." + tld, Suggestions: []domain.Suggestion{}}

	now := s.now()
	found := map[string]*registrar.Availability{}
	var todo []string
	for _, cand := range candidates {
		if a, ok := s.cache.get(cand.Domain, now); ok {
			found[cand.Domain] = a
		} else {
			todo = append(todo, cand.Domain)
		}
	}
	if len(todo) > 0 {
		cctx, cancel := context.WithTimeout(ctx, suggestBudget)
		fresh := s.checkMany(cctx, todo)
		cancel()
		for d, a := range fresh {
			found[d] = a
			s.cache.put(d, a, now)
		}
	}

	for _, cand := range candidates {
		q := domain.Quote{Domain: cand.Domain, Currency: c.Currency, Ordering: s.Ordering}
		a, checked := found[cand.Domain]
		switch {
		case !checked:
			q.Reason = "unknown"
		case a.Unsupported:
			continue // extension non vendue : rien à proposer
		default:
			held, err := s.Store.Held(ctx, cand.Domain)
			if err != nil {
				return nil, err
			}
			if held {
				q.Reason = "taken"
			} else {
				priceQuote(&q, c, a)
			}
		}
		out.Suggestions = append(out.Suggestions, domain.Suggestion{Quote: q, Kind: cand.Kind})
	}
	if len(found) < len(candidates) {
		s.log().Warnw("domains: some suggestions not checked in time", "query", out.Query,
			"checked", len(found), "candidates", len(candidates))
	}
	return out, nil
}

// checkMany : en un appel groupé si le registraire le permet, sinon par
// vérifications parallèles bornées. Les échecs sont simplement absents.
func (s *Service) checkMany(ctx context.Context, names []string) map[string]*registrar.Availability {
	if b, ok := s.Registrar.(registrar.BatchChecker); ok {
		return b.CheckMany(ctx, names)
	}
	out := make(map[string]*registrar.Availability, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, fallbackConcurrency)
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if a, err := s.Registrar.Check(ctx, n); err == nil {
				mu.Lock()
				out[n] = a
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return out
}
