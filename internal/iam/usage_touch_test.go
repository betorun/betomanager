package iam

import (
	"context"
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

type touchSpawner struct{ names []string }

func (s *touchSpawner) Go(name string, _ func(context.Context)) { s.names = append(s.names, name) }

// L'écriture de last_used_at passe par le Spawner (suivie par l'arrêt), une
// fois par minute et par clé.
func TestTouchUsage_UsesSpawner(t *testing.T) {
	sp := &touchSpawner{}
	now := time.Unix(1_000_000, 0)
	s := &ServiceAccountService{lastUsed: map[string]time.Time{}, now: func() time.Time { return now }, Spawner: sp}
	s.touchUsage("k", "41.82.1.2")
	s.touchUsage("k", "41.82.1.2")
	if len(sp.names) != 1 || sp.names[0] != "key-usage" {
		t.Fatalf("spawner = %v", sp.names)
	}
}
