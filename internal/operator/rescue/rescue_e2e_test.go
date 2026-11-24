package rescue_test

// Accès de secours sur un vrai Postgres. Ignoré sans OPERATOR_E2E_DSN
// (bash scripts/operator_e2e_db.sh).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/core/totp"
	"github.com/thekrauss/kubemanager/internal/operator/rescue"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

func TestE2E_Rescue(t *testing.T) {
	dsn := os.Getenv("OPERATOR_E2E_DSN")
	if dsn == "" {
		t.Skip("OPERATOR_E2E_DSN not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	svc, err := rescue.New(db, base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	svc.Now = func() time.Time { return clock }

	email := "e2e-" + time.Now().Format("150405.000000") + "@betorun.test"
	p, err := svc.Prepare(email, "un mot de passe assez long")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := totp.Encoding.DecodeString(strings.ReplaceAll(p.Secret, " ", ""))
	if err := svc.Commit(ctx, p, totp.Code(secret, totp.Step(clock))); err != nil {
		t.Fatal(err)
	}
	p2, _ := svc.Prepare(email, "un mot de passe assez long")
	if err := svc.Commit(ctx, p2, totp.Code(secretOf(p2), totp.Step(clock))); !errors.Is(err, store.ErrExists) {
		t.Fatalf("doublon : %v", err)
	}

	code := totp.Code(secret, totp.Step(clock))
	token, _, _, err := svc.Login(ctx, email, "un mot de passe assez long", code, "panne de l'API, connexion impossible", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Login(ctx, email, "un mot de passe assez long", code, "panne de l'API, connexion impossible", ""); err == nil {
		t.Fatal("code rejoué accepté (base)")
	}
	if a, err := svc.Session(ctx, token); err != nil || a.Email != email {
		t.Fatal(err)
	}
	for i := 0; i < rescue.MaxFailures; i++ {
		_, _, _, err = svc.Login(ctx, email, "faux", "000000", "panne de l'API, connexion impossible", "")
	}
	if !errors.Is(err, rescue.ErrLocked) {
		t.Fatalf("verrouillage (base) : %v", err)
	}
	if err := db.DisableRescueAccount(ctx, email); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Session(ctx, token); err == nil {
		t.Fatal("session d'un compte désactivé acceptée")
	}
	entries, err := db.JournalEntries(ctx, 50)
	if err != nil || len(entries) == 0 {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range entries {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"rescue.account.added", "rescue.login", "rescue.login.failed"} {
		if !kinds[k] {
			t.Fatalf("journal sans %s : %v", k, kinds)
		}
	}
}

func secretOf(p *rescue.Pending) []byte {
	s, _ := totp.Encoding.DecodeString(strings.ReplaceAll(p.Secret, " ", ""))
	return s
}
