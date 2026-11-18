package sts

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestCheckDelegation(t *testing.T) {
	now := time.Now()
	ok := Claims{
		RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute))},
		Method:           MethodDelegated, SessionID: uuid.NewString(),
		Act: &Actor{Subject: ActorCopilot}, Scope: ScopeRead,
	}
	if err := checkDelegation(ok); err != nil {
		t.Fatalf("jeton délégué valide refusé : %v", err)
	}
	bad := map[string]func(c *Claims){
		"sans agent":        func(c *Claims) { c.Act = nil },
		"agent inconnu":     func(c *Claims) { c.Act = &Actor{Subject: "evil"} },
		"portée inconnue":   func(c *Claims) { c.Scope = "admin" },
		"sans session":      func(c *Claims) { c.SessionID = "" },
		"auth_time présent": func(c *Claims) { c.AuthTime = now.Unix() }, // ouvrirait le step-up
		"amr présent":       func(c *Claims) { c.AMR = []string{"otp"} },
		"trop long":         func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour)) },
	}
	for name, mut := range bad {
		c := ok
		mut(&c)
		if checkDelegation(c) == nil {
			t.Errorf("%s : doit être refusé", name)
		}
	}
	// Un jeton non délégué ne peut pas porter act ni scope (pas de délégation
	// fabriquée en ajoutant un claim à un jeton de session).
	session := Claims{Method: MethodSession, Act: &Actor{Subject: ActorCopilot}}
	if checkDelegation(session) == nil {
		t.Error("session avec act : doit être refusée")
	}
	if checkDelegation(Claims{Method: MethodSession, Scope: ScopeWrite}) == nil {
		t.Error("session avec scope : doit être refusée")
	}
}

func TestMintDelegatedRejects(t *testing.T) {
	s := &Service{}
	u, sid := uuid.New(), uuid.New()
	for name, d := range map[string]Delegation{
		"sans personne":   {SessionID: sid, Actor: ActorCopilot, Scope: ScopeRead},
		"sans session":    {UserID: u, Actor: ActorCopilot, Scope: ScopeRead},
		"agent inconnu":   {UserID: u, SessionID: sid, Actor: "x", Scope: ScopeRead},
		"portée inconnue": {UserID: u, SessionID: sid, Actor: ActorCopilot, Scope: "all"},
	} {
		if _, _, err := s.MintDelegated(d); err == nil {
			t.Errorf("%s : erreur attendue", name)
		}
	}
}
