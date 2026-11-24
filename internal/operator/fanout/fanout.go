// Package fanout : interroger plusieurs cibles à la fois (serveurs,
// clusters, sources Prometheus), chacune avec son propre délai. Une cible
// en panne ne consomme pas le délai des autres : en incident, c'est la
// différence entre « server-2 injoignable » et « tout est injoignable ».
package fanout

import (
	"context"
	"sync"
	"time"
)

// Limit : appels simultanés au plus.
const Limit = 8

// Each : f pour chaque élément, au plus Limit à la fois, chacun borné par
// timeout (et par ctx). Les résultats gardent l'ordre des éléments.
func Each[T, R any](ctx context.Context, items []T, timeout time.Duration, f func(context.Context, T) R) []R {
	out := make([]R, len(items))
	sem := make(chan struct{}, Limit)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = f(ctx, it) // contexte échu : f rend son erreur aussitôt
				return
			}
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out[i] = f(c, it)
		})
	}
	wg.Wait()
	return out
}
