package llm

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"
)

// BreakerConfig : Failures pannes de suite ouvrent le disjoncteur pendant
// Cooldown ; ensuite UN appel d'essai passe, qui le referme ou le rouvre.
type BreakerConfig struct {
	Failures int
	Cooldown time.Duration
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.Failures <= 0 {
		c.Failures = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 30 * time.Second
	}
	return c
}

// Breaker : disjoncteur autour d'un fournisseur. Évite d'attendre un délai
// d'expiration à chaque message quand un fournisseur est tombé : la chaîne
// passe directement au suivant. État en mémoire, par processus (un
// fournisseur en panne l'est pour toutes les instances, chacune le découvre).
type Breaker struct {
	next Provider
	cfg  BreakerConfig
	now  func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
}

func WithBreaker(p Provider, cfg BreakerConfig) *Breaker {
	return &Breaker{next: p, cfg: cfg.withDefaults(), now: time.Now}
}

func (b *Breaker) Name() string { return b.next.Name() }

// admit : l'appel peut-il passer ? probe=true pour l'appel d'essai.
func (b *Breaker) admit() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return false, nil
	}
	if b.now().Before(b.openUntil) || b.probing {
		return false, &Error{Kind: KindCircuitOpen, Provider: b.next.Name(), Message: "fournisseur mis de côté après des pannes répétées"}
	}
	b.probing = true
	return true, nil
}

func (b *Breaker) record(probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	var e *Error
	failed := err != nil && errors.As(err, &e) && e.countsAsFailure()
	if !failed {
		if err == nil || probe {
			// Succès, ou essai interrompu pour une raison étrangère au
			// fournisseur : on referme (un essai raté pour de bon rouvre).
			b.failures = 0
			if err == nil {
				b.openUntil = time.Time{}
			}
		}
		return
	}
	b.failures++
	if probe || b.failures >= b.cfg.Failures {
		b.openUntil = b.now().Add(b.cfg.Cooldown)
	}
}

func (b *Breaker) Generate(ctx context.Context, req Request) (*Response, error) {
	probe, err := b.admit()
	if err != nil {
		return nil, err
	}
	resp, err := b.next.Generate(ctx, req)
	b.record(probe, err)
	return resp, err
}

func (b *Breaker) Stream(ctx context.Context, req Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		probe, err := b.admit()
		if err != nil {
			yield(Event{}, err)
			return
		}
		var final error
		recorded := false
		defer func() {
			if !recorded {
				// Boucle interrompue par l'appelant : ni succès ni panne.
				b.record(probe, context.Canceled)
			}
		}()
		for ev, err := range b.next.Stream(ctx, req) {
			if err != nil {
				final = err
				break
			}
			if ev.Kind == EventDone {
				b.record(probe, nil)
				recorded = true
			}
			if !yield(ev, nil) {
				return
			}
		}
		if final != nil {
			b.record(probe, final)
			recorded = true
			yield(Event{}, final)
		}
	}
}
