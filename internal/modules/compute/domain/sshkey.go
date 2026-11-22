package domain

import (
	"crypto/rsa"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHKey : clé publique du client, installée à la création (jamais de mot de passe).
type SSHKey struct {
	// Line : clé normalisée « type base64 » (commentaire retiré).
	Line        string
	Fingerprint string // SHA256:…
}

// ParseSSHKey : une seule clé, au format authorized_keys.
func ParseSSHKey(s string) (*SSHKey, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n\r") || len(s) > 16<<10 {
		return nil, ErrInvalidSSHKey
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, ErrInvalidSSHKey
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	case ssh.KeyAlgoRSA:
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, ErrInvalidSSHKey
		}
		if k, ok := ck.CryptoPublicKey().(*rsa.PublicKey); !ok || k.N.BitLen() < 2048 {
			return nil, ErrInvalidSSHKey
		}
	default:
		return nil, ErrInvalidSSHKey
	}
	return &SSHKey{Line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), Fingerprint: ssh.FingerprintSHA256(pub)}, nil
}
