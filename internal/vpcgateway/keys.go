package vpcgateway

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/curve25519"
)

// ValidKey : clé du tunnel (32 octets en base64 standard, 44 caractères).
func ValidKey(k string) bool {
	if len(k) != 44 {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(k)
	return err == nil && len(b) == 32
}

// GenerateKey : paire de clés de la passerelle (Curve25519, comme « wg
// genkey » puis « wg pubkey »). La clé privée ne quitte que vers le Secret
// de la passerelle.
func GenerateKey() (private, public string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(pub), nil
}

// PublicKey : clé publique d'une clé privée (base64).
func PublicKey(private string) (string, error) {
	k, err := base64.StdEncoding.DecodeString(private)
	if err != nil {
		return "", err
	}
	pub, err := curve25519.X25519(k, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// NewStatusToken : jeton aléatoire de GET /status (32 octets, base64 URL).
func NewStatusToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
