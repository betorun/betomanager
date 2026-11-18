package llm

import "time"

// SetBreakerClock : horloge du disjoncteur, pour les tests.
func SetBreakerClock(b *Breaker, now func() time.Time) { b.now = now }
