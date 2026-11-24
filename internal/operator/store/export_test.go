package store

import "time"

// SetNow : horloge des tests.
func (s *Store) SetNow(f func() time.Time) { s.now = f }
