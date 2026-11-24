// Package totp : codes à usage unique basés sur le temps (RFC 6238) :
// 6 chiffres, pas de 30 s, HMAC-SHA1 — les réglages que toutes les
// applications d'authentification comprennent. Partagé par la connexion de
// la console (internal/auth) et l'accès de secours de l'Opérateur.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 : algorithme de RFC 6238, seul compris par toutes les applications.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Digits     = 6
	Period     = 30
	SecretSize = 20 // 160 bits (RFC 4226 §4)
	// Skew : pas tolérés de part et d'autre (horloge du téléphone).
	Skew = 1
)

// Encoding : base32 sans bourrage (format des applications).
var Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret : secret aléatoire de 160 bits.
func NewSecret() ([]byte, error) {
	b := make([]byte, SecretSize)
	_, err := rand.Read(b)
	return b, err
}

// Step : numéro du pas de 30 s à l'instant t.
func Step(t time.Time) int64 { return t.Unix() / Period }

// Code : code du pas step.
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, v%1_000_000)
}

// Match : pas accepté pour code (0 : refusé). Les pas déjà utilisés
// (≤ lastUsed) sont refusés : un code ne sert qu'une fois.
func Match(secret []byte, code string, now time.Time, lastUsed int64) int64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0
	}
	current := Step(now)
	for d := -Skew; d <= Skew; d++ {
		step := current + int64(d)
		if step <= lastUsed {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1 {
			return step
		}
	}
	return 0
}

// URI : URI otpauth:// à afficher en QR code (format Google Authenticator).
func URI(secret []byte, issuer, account string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", Encoding.EncodeToString(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Format : secret en base32 par groupes de 4, pour une saisie manuelle.
func Format(secret []byte) string {
	s := Encoding.EncodeToString(secret)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
