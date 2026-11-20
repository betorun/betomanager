package sts

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// Le JWK publié doit redonner exactement la clé : coordonnées sur 32 octets,
// complétées à gauche (une coordonnée courte arrive environ 1 fois sur 128).
func TestPublicJWK_RoundTrip(t *testing.T) {
	for i := 0; i < 300; i++ {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		jwk, err := publicJWK("kid", &priv.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) != 32 {
			t.Fatalf("x: len=%d err=%v", len(x), err)
		}
		y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
		if err != nil || len(y) != 32 {
			t.Fatalf("y: len=%d err=%v", len(y), err)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := priv.PublicKey.Bytes()
		got, _ := pub.Bytes()
		if !bytes.Equal(want, got) {
			t.Fatal("JWK ne redonne pas la clé publique")
		}
		if jwk.Kty != "EC" || jwk.Crv != "P-256" {
			t.Fatalf("kty/crv: %s/%s", jwk.Kty, jwk.Crv)
		}
	}
}

// Une clé d'une autre courbe (ligne corrompue en base) : erreur, pas de
// panique (FillBytes paniquait sur une coordonnée de plus de 32 octets).
func TestPublicJWK_RejectsOtherCurves(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publicJWK("kid", &priv.PublicKey); err == nil {
		t.Fatal("clé P-384 acceptée")
	}
}
