package rescue

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/totp"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

type mem struct {
	mu       sync.Mutex
	accounts map[string]*store.RescueAccount
	sessions map[string]uuid.UUID
	journal  []string
	now      func() time.Time
}

// alerter : garde les alertes (envoyées sans attendre par le service).
type alerter struct{ c chan string }

func (a *alerter) Alert(_ context.Context, text string) { a.c <- text }

func (a *alerter) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-a.c:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("aucune alerte")
		return ""
	}
}

func newMem(now func() time.Time) *mem {
	return &mem{accounts: map[string]*store.RescueAccount{}, sessions: map[string]uuid.UUID{}, now: now}
}

func (m *mem) CreateRescueAccount(_ context.Context, a store.RescueAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[a.Email]; ok {
		return store.ErrExists
	}
	m.accounts[a.Email] = &a
	return nil
}

func (m *mem) RescueAccountByEmail(_ context.Context, email string) (store.RescueAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[email]
	if !ok {
		return store.RescueAccount{}, store.ErrNotFound
	}
	return *a, nil
}

func (m *mem) byID(id uuid.UUID) *store.RescueAccount {
	for _, a := range m.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (m *mem) RescueFailure(_ context.Context, id uuid.UUID, max int, lock time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID(id)
	a.FailedAttempts++
	if a.FailedAttempts >= max {
		a.FailedAttempts = 0
		t := m.now().Add(lock)
		a.LockedUntil = &t
	}
	return a.LockedUntil != nil && a.LockedUntil.After(m.now()), nil
}

func (m *mem) RescueSuccess(_ context.Context, id uuid.UUID, step int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID(id)
	if a.TOTPLastStep >= step {
		return false, nil
	}
	a.TOTPLastStep, a.FailedAttempts, a.LockedUntil = step, 0, nil
	return true, nil
}

func (m *mem) CreateRescueSession(_ context.Context, token string, id uuid.UUID, ttl time.Duration) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = id
	return m.now().Add(ttl), nil
}

func (m *mem) RescueSession(_ context.Context, token string) (store.RescueAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.sessions[token]
	if !ok {
		return store.RescueAccount{}, store.ErrNotFound
	}
	return *m.byID(id), nil
}

func (m *mem) RevokeRescueSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *mem) Journal(_ context.Context, actor, kind string, _ *uuid.UUID, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal = append(m.journal, actor+" "+kind)
	return nil
}

func key() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func secretOf(t *testing.T, p *Pending) []byte {
	t.Helper()
	s, err := totp.Encoding.DecodeString(strings.ReplaceAll(p.Secret, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEnrollAndLogin(t *testing.T) {
	clock := time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	m := newMem(now)
	s, err := New(m, key())
	if err != nil {
		t.Fatal(err)
	}
	s.Now = now
	alerts := &alerter{c: make(chan string, 16)}
	s.Alert = alerts
	ctx := context.Background()

	if _, err := s.Prepare("awa@betorun.test", "court"); !errors.Is(err, ErrWeakPassword) {
		t.Fatal(err)
	}
	if _, err := s.Prepare("pas un e-mail", "un mot de passe assez long"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatal(err)
	}
	p, err := s.Prepare(" Awa@Betorun.test ", "un mot de passe assez long")
	if err != nil || !strings.HasPrefix(p.URI, "otpauth://totp/") {
		t.Fatal(p, err)
	}
	secret := secretOf(t, p)
	if err := s.Commit(ctx, p, "000000"); !errors.Is(err, ErrBadCode) && totp.Code(secret, totp.Step(clock)) != "000000" {
		t.Fatal("code faux accepté à l'inscription")
	}
	if err := s.Commit(ctx, p, totp.Code(secret, totp.Step(clock))); err != nil {
		t.Fatal(err)
	}
	acc := m.accounts["awa@betorun.test"]
	if strings.Contains(string(acc.TOTPSecret), string(secret)) || !strings.HasPrefix(acc.PasswordHash, "$argon2id$") {
		t.Fatal("secret ou mot de passe en clair")
	}

	// Le code de l'inscription ne compte pas comme utilisé : pas suivant.
	clock = clock.Add(30 * time.Second)
	code := totp.Code(secret, totp.Step(clock))
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "mauvais mot de passe!!", code, "panne de l'API, connexion impossible", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("mauvais mot de passe accepté")
	}
	// Motif absent ou trop court : refusé avant toute vérification (le code
	// n'est pas consommé).
	for _, r := range []string{"", "   ", "panne"} {
		if _, _, _, err := s.Login(ctx, "awa@betorun.test", "un mot de passe assez long", code, r, ""); !errors.Is(err, ErrReason) {
			t.Fatalf("motif %q : %v", r, err)
		}
	}
	token, exp, got, err := s.Login(ctx, "AWA@betorun.test", "un mot de passe assez long", code, "panne de l'API, connexion impossible", "1.2.3.4")
	if err != nil || !strings.HasPrefix(token, TokenPrefix) || got.Email != "awa@betorun.test" || !exp.Equal(clock.Add(SessionTTL)) {
		t.Fatal(token, err)
	}
	if a := alerts.next(t); !strings.Contains(a, "SECOURS de awa@betorun.test") || !strings.Contains(a, "Motif : panne de l'API") {
		t.Fatalf("alerte = %q", a)
	}
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "un mot de passe assez long", code, "panne de l'API, connexion impossible", ""); err == nil {
		t.Fatal("code rejoué accepté")
	}
	if a, err := s.Session(ctx, token); err != nil || a.ID != acc.ID {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "rsc_inconnu"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "jeton-de-l-api"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if err := s.Logout(ctx, token, got.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token); err == nil {
		t.Fatal("session révoquée acceptée")
	}

	// Compte inconnu : même erreur.
	if _, _, _, err := s.Login(ctx, "inconnu@betorun.test", "x", "123456", "panne de l'API, connexion impossible", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}

	// Verrouillage après 5 échecs (le code rejoué plus haut en était un),
	// levé après 15 minutes.
	for i := 0; i < MaxFailures-2; i++ {
		if _, _, _, err := s.Login(ctx, "awa@betorun.test", "faux", "000000", "panne de l'API, connexion impossible", ""); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("échec %d : %v", i, err)
		}
	}
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "faux", "000000", "panne de l'API, connexion impossible", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("verrouillage : %v", err)
	}
	if a := alerts.next(t); !strings.Contains(a, "verrouillé") {
		t.Fatalf("alerte = %q", a)
	}
	clock = clock.Add(time.Minute)
	good := totp.Code(secret, totp.Step(clock))
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "un mot de passe assez long", good, "panne de l'API, connexion impossible", ""); !errors.Is(err, ErrLocked) {
		t.Fatal("bons identifiants acceptés pendant le verrouillage")
	}
	clock = clock.Add(LockFor)
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "un mot de passe assez long", totp.Code(secret, totp.Step(clock)), "panne de l'API, connexion impossible", ""); err != nil {
		t.Fatalf("après verrouillage : %v", err)
	}

	// Compte désactivé.
	d := clock
	acc.DisabledAt = &d
	clock = clock.Add(30 * time.Second)
	if _, _, _, err := s.Login(ctx, "awa@betorun.test", "un mot de passe assez long", totp.Code(secret, totp.Step(clock)), "panne de l'API, connexion impossible", ""); err == nil {
		t.Fatal("compte désactivé accepté")
	}
	if !strings.Contains(strings.Join(m.journal, "\n"), "rescue:awa@betorun.test rescue.login.locked") {
		t.Fatalf("journal = %v", m.journal)
	}
}

// Un secret recopié sur un autre compte ne se déchiffre plus.
func TestSecretBoundToAccount(t *testing.T) {
	s, _ := New(newMem(time.Now), key())
	sealed, err := s.seal(uuid.New(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.open(uuid.New(), sealed); err == nil {
		t.Fatal("secret ouvert avec un autre compte")
	}
	if _, err := New(nil, "court"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal(err)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("un mot de passe assez long")
	if err != nil || !VerifyPassword(h, "un mot de passe assez long") || VerifyPassword(h, "un mot de passe assez lonG") {
		t.Fatal("argon2id")
	}
	for _, bad := range []string{"", "$bcrypt$x", "$argon2id$v=19$m=999999999,t=3,p=2$AAAA$AAAA", h[:len(h)-4]} {
		if VerifyPassword(bad, "x") {
			t.Fatalf("condensé mal formé accepté : %q", bad)
		}
	}
}
