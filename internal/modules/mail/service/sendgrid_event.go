package service

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// En-têtes de signature du webhook d'événements SendGrid.
const (
	SendGridSignatureHeader = "X-Twilio-Email-Event-Webhook-Signature"
	SendGridTimestampHeader = "X-Twilio-Email-Event-Webhook-Timestamp"
)

// ParseSendGridPublicKey lit la clé de vérification du tableau de bord SendGrid
// (base64, DER).
func ParseSendGridPublicKey(encoded string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("sendgrid public key is not ECDSA")
	}
	return pub, nil
}

// VerifySendGridSignature : SendGrid signe l'horodatage suivi du corps brut.
func VerifySendGridSignature(key *ecdsa.PublicKey, signature, timestamp string, body []byte) bool {
	if key == nil || signature == "" || timestamp == "" {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(append([]byte(timestamp), body...))
	return ecdsa.VerifyASN1(key, hash[:], sig)
}

// ParseSendGridBouncedEmails extrait les adresses définitivement refusées. Un
// « blocked » n'en fait pas partie : la cause est l'expéditeur ou le contenu, et
// l'adresse reste valable. Un « dropped » compte seulement quand SendGrid écarte
// l'adresse elle-même.
func ParseSendGridBouncedEmails(body []byte) ([]string, error) {
	var events []struct {
		Email  string `json:"email"`
		Event  string `json:"event"`
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(events))
	emails := make([]string, 0, len(events))
	for _, e := range events {
		email := strings.ToLower(strings.TrimSpace(e.Email))
		if email == "" {
			continue
		}
		bounced := (e.Event == "bounce" && e.Type != "blocked") ||
			(e.Event == "dropped" && (e.Reason == "Bounced Address" || e.Reason == "Invalid"))
		if !bounced {
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		emails = append(emails, email)
	}
	return emails, nil
}
