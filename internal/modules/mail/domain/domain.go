// Package domain : envoi des e-mails de betorun (docs/modules/mail.md). Les
// mails partent par une file (asynq, Redis), avec des reprises ; une adresse
// refusée par le serveur ou signalée par SendGrid (rebond, plainte) est mise
// de côté : plus rien ne lui est envoyé tant que l'équipe ne la relâche pas.
package domain

import (
	"errors"
	"net/textproto"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

// Raisons et origines d'une mise de côté.
const (
	ReasonRejected  = "rejected"  // refus définitif du serveur SMTP (550, 551, 553)
	ReasonBounce    = "bounce"    // rebond signalé par SendGrid
	ReasonComplaint = "complaint" // le destinataire a signalé le mail comme indésirable
	SourceSMTP      = "smtp"
	SourceSendGrid  = "sendgrid"
)

// Suppression : adresse à qui plus rien n'est envoyé.
type Suppression struct {
	Email     string    `gorm:"primaryKey" json:"email"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (Suppression) TableName() string { return "mail_suppressions" }

// maxDetail : début du message du serveur gardé pour l'équipe.
const maxDetail = 300

// NewSuppression : adresse normalisée, détail borné.
func NewSuppression(email, reason, source, detail string) Suppression {
	if len(detail) > maxDetail {
		detail = detail[:maxDetail]
	}
	return Suppression{Email: NormalizeEmail(email), Reason: reason, Source: source, Detail: detail}
}

// NormalizeEmail : minuscules, sans espaces ni nom affiché.
func NormalizeEmail(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '<'); i >= 0 {
		s = strings.TrimSuffix(s[i+1:], ">")
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// Outcome : suite à donner à un envoi.
type Outcome int

const (
	Sent     Outcome = iota
	Retry            // panne passagère ou configuration à corriger : réessayer plus tard
	Rejected         // l'adresse est refusée pour de bon : la mettre de côté
	Failed           // le message lui-même est refusé : archiver sans réessayer
)

// Classify : erreurs de net/smtp (*textproto.Error, codes 4xx et 5xx), du
// réseau ou du message. Un 5xx d'authentification (530, 535) ou de politique
// d'envoi (réputation, volume, expéditeur) n'est pas un refus de l'adresse :
// c'est à betorun de corriger, on réessaie.
func Classify(err error) Outcome {
	if err == nil {
		return Sent
	}
	if mail.IsInvalid(err) {
		return Failed
	}
	var te *textproto.Error
	if !errors.As(err, &te) {
		return Retry // connexion, délai, TLS
	}
	switch {
	case te.Code >= 400 && te.Code < 500:
		return Retry
	case te.Code == 530 || te.Code == 535 || (te.Code == 554 && senderSide(te.Msg)):
		return Retry
	case te.Code == 550 || te.Code == 551 || te.Code == 553:
		if senderSide(te.Msg) {
			return Retry
		}
		return Rejected
	case te.Code >= 500:
		return Failed
	}
	return Retry
}

// senderSide : le refus vise l'expéditeur, sa réputation ou le volume, pas l'adresse.
func senderSide(msg string) bool {
	m := strings.ToLower(msg)
	for _, n := range []string{"5.7.", "5.4.", "5.1.7", "5.1.8", "5.3.", "sender", "from address", "relay",
		"reputation", "blocked", "blacklist", "blocklist", "policy", "spam", "limit", "quota", "rate",
		"unauthenticated", "authentication", "dmarc", "spf", "dkim"} {
		if strings.Contains(m, n) {
			return true
		}
	}
	return false
}

// MaskEmail : « a***@ex.sn » pour les journaux (pas d'adresse complète).
func MaskEmail(email string) string {
	e := NormalizeEmail(email)
	at := strings.LastIndexByte(e, '@')
	if at <= 0 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}
