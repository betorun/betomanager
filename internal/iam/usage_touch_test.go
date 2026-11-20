package iam

import (
	"fmt"
	"testing"
	"time"
)

func TestShouldTouch_ThrottlesPerKey(t *testing.T) {
	s := &ServiceAccountService{lastUsed: map[string]time.Time{}}
	t0 := time.Unix(1_000_000, 0)
	if !s.shouldTouch("k", t0) {
		t.Fatal("première utilisation non enregistrée")
	}
	if s.shouldTouch("k", t0.Add(30*time.Second)) {
		t.Fatal("deux écritures dans la même minute")
	}
	if !s.shouldTouch("k", t0.Add(61*time.Second)) {
		t.Fatal("pas d'écriture après une minute")
	}
}

// Les clés qui ne servent plus (révoquées, expirées) sortent de la table :
// sans balayage, elle grossissait pendant toute la vie du process.
func TestShouldTouch_SweepsStaleKeys(t *testing.T) {
	s := &ServiceAccountService{lastUsed: map[string]time.Time{}}
	t0 := time.Unix(1_000_000, 0)
	for i := 0; i < 5000; i++ {
		s.shouldTouch(fmt.Sprintf("old-%d", i), t0)
	}
	s.shouldTouch("fresh", t0.Add(2*time.Minute))
	if n := len(s.lastUsed); n != 1 {
		t.Fatalf("table après balayage : %d entrées, attendu 1", n)
	}
}
