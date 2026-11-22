package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
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

// ParseSendGridSuppressions : adresses à mettre de côté d'après un lot
// d'événements. Un « blocked » n'en fait pas partie (la cause est
// l'expéditeur ou le contenu, l'adresse reste valable) ; un « dropped »
// seulement quand SendGrid écarte l'adresse elle-même ; un « spamreport » :
// le destinataire ne veut plus de nos mails.
func ParseSendGridSuppressions(body []byte) ([]domain.Suppression, error) {
	var events []struct {
		Email  string `json:"email"`
		Event  string `json:"event"`
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []domain.Suppression{}
	for _, e := range events {
		reason := ""
		switch {
		case e.Event == "bounce" && e.Type != "blocked",
			e.Event == "dropped" && (e.Reason == "Bounced Address" || e.Reason == "Invalid"):
			reason = domain.ReasonBounce
		case e.Event == "spamreport":
			reason = domain.ReasonComplaint
		default:
			continue
		}
		s := domain.NewSuppression(e.Email, reason, domain.SourceSendGrid, e.Reason)
		if s.Email == "" || seen[s.Email] {
			continue
		}
		seen[s.Email] = true
		out = append(out, s)
	}
	return out, nil
}

// SendGridWebhook : événements signés de SendGrid → adresses mises de côté.
type SendGridWebhook struct {
	Key          *ecdsa.PublicKey
	Suppressions Suppressions
}

// ErrBadSignature : signature absente ou fausse.
var ErrBadSignature = errors.New("invalid SendGrid signature")

// sendGridMaxSkew : horodatage accepté à cinq minutes près (rejeu).
const sendGridMaxSkew = 5 * time.Minute

func (w *SendGridWebhook) Handle(ctx context.Context, signature, timestamp string, body []byte, now time.Time) (int, error) {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || now.Sub(time.Unix(ts, 0)).Abs() > sendGridMaxSkew || !VerifySendGridSignature(w.Key, signature, timestamp, body) {
		return 0, ErrBadSignature
	}
	list, err := ParseSendGridSuppressions(body)
	if err != nil {
		return 0, err
	}
	for _, s := range list {
		if err := w.Suppressions.Suppress(ctx, s); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}
