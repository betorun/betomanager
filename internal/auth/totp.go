package auth

import (
	"time"

	"github.com/thekrauss/kubemanager/internal/core/totp"
)

// TOTP de la console : implémentation commune dans internal/core/totp.

// TOTPIssuer : nom affiché dans l'application d'authentification.
const TOTPIssuer = "betorun"

var totpEncoding = totp.Encoding

func newTOTPSecret() ([]byte, error)            { return totp.NewSecret() }
func totpStep(t time.Time) int64                { return totp.Step(t) }
func totpCode(secret []byte, step int64) string { return totp.Code(secret, step) }
func totpMatch(secret []byte, code string, now time.Time, lastUsed int64) int64 {
	return totp.Match(secret, code, now, lastUsed)
}

// TOTPURI : URI otpauth:// à afficher en QR code (format Google Authenticator).
func TOTPURI(secret []byte, account string) string { return totp.URI(secret, TOTPIssuer, account) }

// FormatTOTPSecret : secret en base32 par groupes de 4, pour une saisie manuelle.
func FormatTOTPSecret(secret []byte) string { return totp.Format(secret) }
