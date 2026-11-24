package opjournal

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	keys, err := ParsePublicKeys([]string{base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(other), ""})
	if err != nil || len(keys) != 2 {
		t.Fatal(keys, err)
	}
	now := time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"entries":[]}`)
	h := Sign(priv, now, body)
	verify := func(body []byte, at time.Time, h map[string]string) error {
		return Verify(keys, h[HeaderKey], h[HeaderTimestamp], h[HeaderSignature], body, at)
	}
	if err := verify(body, now.Add(4*time.Minute), h); err != nil {
		t.Fatal(err)
	}
	if verify([]byte(`{"entries":[{}]}`), now, h) == nil {
		t.Fatal("corps modifié accepté")
	}
	if verify(body, now.Add(6*time.Minute), h) == nil || verify(body, now.Add(-6*time.Minute), h) == nil {
		t.Fatal("horodatage hors fenêtre accepté")
	}
	forged := map[string]string{HeaderKey: KeyID(other), HeaderTimestamp: h[HeaderTimestamp], HeaderSignature: h[HeaderSignature]}
	if verify(body, now, forged) == nil {
		t.Fatal("signature d'une autre clé acceptée")
	}
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	if verify(body, now, Sign(stranger, now, body)) == nil {
		t.Fatal("clé inconnue acceptée")
	}
	if _, err := ParsePublicKeys([]string{"Y291cnQ="}); err == nil {
		t.Fatal("clé courte acceptée")
	}
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	if k, err := ParsePrivateKey(seed); err != nil || !k.Equal(priv) {
		t.Fatal(err)
	}
}
