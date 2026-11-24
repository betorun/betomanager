package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	netmail "net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

// Webhook « E-mail » : une adresse de plus (liste d'équipe, astreinte, Gmail
// d'une personne sans compte). Elle ne reçoit rien avant d'avoir confirmé par
// le lien envoyé : on ne peut pas s'en servir pour écrire à n'importe qui.
// Chaque alerte porte un lien qui retire l'adresse en un clic.

// WebhookEmail : sorte de webhook « E-mail ».
const WebhookEmail = "email"

// Délais de la confirmation.
const (
	// EmailConfirmTTL : validité du lien de confirmation.
	EmailConfirmTTL = 7 * 24 * time.Hour
	// EmailResendEvery : au plus un nouvel e-mail de confirmation par période.
	EmailResendEvery = 10 * time.Minute
	// StatusPending : adresse pas encore confirmée.
	StatusPending = "pending"
)

// Erreurs de l'e-mail.
var (
	ErrEmailAddress     = errors.New("the email address is invalid")
	ErrEmailUnavailable = errors.New("email alerts are not available on this platform")
	ErrEmailLink        = errors.New("this link is invalid or has expired")
)

// EmailLinkRequest : jeton reçu par e-mail (confirmation ou retrait).
type EmailLinkRequest struct {
	Token string `json:"token" form:"token" description:"Jeton du lien reçu par e-mail"`
}

// EmailLinkResult : projet et adresse concernés.
type EmailLinkResult struct {
	Project string `json:"project"`
	Address string `json:"address"`
}

// validEmail : une seule adresse, sans nom affiché, en minuscules.
func validEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	a, err := netmail.ParseAddress(raw)
	if err != nil || a.Address != raw || len(raw) > 254 || !strings.Contains(raw[strings.LastIndex(raw, "@")+1:], ".") {
		return "", ErrEmailAddress
	}
	return strings.ToLower(raw), nil
}

// maskEmail : « a…@entreprise.com » (adresse d'un tiers, montrée en partie).
func maskEmail(addr string) string {
	local, domain, _ := strings.Cut(addr, "@")
	if local == "" {
		return addr
	}
	return string([]rune(local)[:1]) + "…@" + domain
}

// ── Jetons des liens ─────────────────────────────────────────────────────

func emailSign(key []byte, purpose, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("alert-email-" + purpose + "\x00" + payload))
	return m.Sum(nil)[:16]
}

// emailToken : « confirm » (expire) ou « remove » (sans date : il ne fait
// que retirer l'adresse).
func emailToken(key []byte, purpose string, id uuid.UUID, exp time.Time) string {
	payload := id.String()
	if !exp.IsZero() {
		payload += "." + strconv.FormatInt(exp.Unix(), 10)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(emailSign(key, purpose, payload))
}

func parseEmailToken(key []byte, purpose, token string, now time.Time) (uuid.UUID, error) {
	p, s, ok := strings.Cut(strings.TrimSpace(token), ".")
	if len(key) == 0 || !ok {
		return uuid.Nil, ErrEmailLink
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, emailSign(key, purpose, string(raw))) {
		return uuid.Nil, ErrEmailLink
	}
	idPart, expPart, hasExp := strings.Cut(string(raw), ".")
	if hasExp {
		exp, err := strconv.ParseInt(expPart, 10, 64)
		if err != nil || now.After(time.Unix(exp, 0)) {
			return uuid.Nil, ErrEmailLink
		}
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return uuid.Nil, ErrEmailLink
	}
	return id, nil
}

// ── Envoi ────────────────────────────────────────────────────────────────

func (s *WebhookService) emailReady() bool {
	return s.Mail != nil && len(s.Key) > 0 && s.ConsoleURL != ""
}

func lang(row webhookRow) i18n.Locale {
	if row.Locale == "en" {
		return i18n.EN
	}
	return i18n.FR
}

var emailCopies = map[i18n.Locale]struct {
	Greeting, Button, Outro, Remove                          string
	ConfirmSubject, ConfirmTitle, ConfirmBody, ConfirmButton string
	ConfirmOutro                                             string
}{
	i18n.FR: {
		Greeting: "Bonjour,", Button: "Ouvrir la console",
		Outro:          "Vous recevez cet e-mail parce que cette adresse a été ajoutée aux alertes du projet « %s ».",
		Remove:         "Ne plus recevoir les alertes de ce projet",
		ConfirmSubject: "Confirmez les alertes du projet « %s »", ConfirmTitle: "Recevoir les alertes de ce projet ?",
		ConfirmBody:   "Quelqu'un a ajouté cette adresse aux alertes du projet « %s » sur betorun : conteneurs en panne, builds échoués, disques pleins, nouvelles clés API. Rien ne sera envoyé sans votre confirmation.",
		ConfirmButton: "Confirmer l'adresse",
		ConfirmOutro:  "Ce n'est pas vous ? Ignorez cet e-mail : l'adresse ne recevra rien. Le lien est valable 7 jours.",
	},
	i18n.EN: {
		Greeting: "Hello,", Button: "Open the console",
		Outro:          "You receive this email because this address was added to the alerts of project \"%s\".",
		Remove:         "Stop receiving this project's alerts",
		ConfirmSubject: "Confirm the alerts of project \"%s\"", ConfirmTitle: "Receive this project's alerts?",
		ConfirmBody:   "Someone added this address to the alerts of project \"%s\" on betorun: failing containers, failed builds, full disks, new API keys. Nothing will be sent without your confirmation.",
		ConfirmButton: "Confirm the address",
		ConfirmOutro:  "Not you? Ignore this email: the address will receive nothing. The link is valid for 7 days.",
	},
}

func (s *WebhookService) emailAddress(row webhookRow) (string, error) {
	raw, err := s.Sealer.Open(row.URLSealed, sealContext(row.ID, "url"))
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(raw), "mailto:"), nil
}

func (s *WebhookService) removeLinks(id uuid.UUID) (page, oneClick string) {
	tok := url.QueryEscape(emailToken(s.Key, "remove", id, time.Time{}))
	page = strings.TrimRight(s.ConsoleURL, "/") + "/alert-email?remove=" + tok
	if s.APIURL != "" {
		oneClick = strings.TrimRight(s.APIURL, "/") + "/alerts/email/remove?token=" + tok
	}
	return page, oneClick
}

func renderEmail(l i18n.Locale, to, subject, title, body, button, link, outro, remove, removeText string) (mail.Message, error) {
	data := map[string]string{"Lang": string(l), "Title": title, "Greeting": emailCopies[l].Greeting, "Body": body,
		"Button": button, "Link": link, "Outro": outro, "Unsubscribe": remove, "UnsubscribeText": removeText}
	var text, html bytes.Buffer
	if err := textLayout.Execute(&text, data); err != nil {
		return mail.Message{}, err
	}
	if err := htmlLayout.Execute(&html, data); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{To: to, Subject: subject, Text: text.String(), HTML: html.String()}, nil
}

// sendEmailAlert : une alerte vers une adresse confirmée.
func (s *WebhookService) sendEmailAlert(ctx context.Context, row webhookRow, ev WebhookEvent) string {
	if !s.emailReady() {
		return "error"
	}
	to, err := s.emailAddress(row)
	if err != nil {
		return "error"
	}
	l := lang(row)
	c := emailCopies[l]
	page, oneClick := s.removeLinks(row.ID)
	msg, err := renderEmail(l, to, ev.Title, ev.Title, ev.Text, c.Button, ev.Link, fmt.Sprintf(c.Outro, ev.Project), page, c.Remove)
	if err != nil {
		return "error"
	}
	msg.Unsubscribe = oneClick
	if err := s.Mail.Send(ctx, msg); err != nil {
		return "unreachable"
	}
	return "ok"
}

// sendConfirmation : lien de confirmation vers l'adresse (au plus un par
// EmailResendEvery) ; l'état reste « pending ».
func (s *WebhookService) sendConfirmation(ctx context.Context, row webhookRow, project string) error {
	if row.LastDeliveryAt != nil && s.now().Sub(*row.LastDeliveryAt) < EmailResendEvery {
		return nil
	}
	to, err := s.emailAddress(row)
	if err != nil {
		return err
	}
	l := lang(row)
	c := emailCopies[l]
	link := strings.TrimRight(s.ConsoleURL, "/") + "/alert-email?confirm=" +
		url.QueryEscape(emailToken(s.Key, "confirm", row.ID, s.now().Add(EmailConfirmTTL)))
	msg, err := renderEmail(l, to, fmt.Sprintf(c.ConfirmSubject, project), c.ConfirmTitle, fmt.Sprintf(c.ConfirmBody, project),
		c.ConfirmButton, link, c.ConfirmOutro, "", "")
	if err != nil {
		return err
	}
	status := StatusPending
	if err := s.Mail.Send(ctx, msg); err != nil {
		status = "unreachable"
	}
	now := s.now()
	return s.DB.WithContext(ctx).Model(&webhookRow{}).Where("id = ?", row.ID).
		Updates(map[string]any{"last_delivery_at": now, "last_status": status}).Error
}

// ── Liens reçus (sans session : le jeton signé suffit) ───────────────────

func (s *WebhookService) linkResult(ctx context.Context, row webhookRow) *EmailLinkResult {
	out := &EmailLinkResult{Address: row.URLHint}
	if s.ProjectName != nil {
		out.Project, _ = s.ProjectName(ctx, row.ProjectID)
	}
	return out
}

// ConfirmEmail : l'adresse a cliqué le lien ; elle recevra les alertes.
func (s *WebhookService) ConfirmEmail(ctx context.Context, token string) (*EmailLinkResult, error) {
	id, err := parseEmailToken(s.Key, "confirm", token, s.now())
	if err != nil {
		return nil, err
	}
	var row webhookRow
	if err := s.DB.WithContext(ctx).Where("id = ? AND kind = ?", id, WebhookEmail).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrEmailLink
		}
		return nil, err
	}
	if row.ConfirmedAt == nil {
		now := s.now()
		if err := s.DB.WithContext(ctx).Model(&webhookRow{}).Where("id = ?", id).
			Updates(map[string]any{"confirmed_at": now, "last_status": ""}).Error; err != nil {
			return nil, err
		}
	}
	return s.linkResult(ctx, row), nil
}

// RemoveEmail : lien « ne plus recevoir » d'une alerte ; rejouable.
func (s *WebhookService) RemoveEmail(ctx context.Context, token string) (*EmailLinkResult, error) {
	id, err := parseEmailToken(s.Key, "remove", token, s.now())
	if err != nil {
		return nil, err
	}
	var row webhookRow
	err = s.DB.WithContext(ctx).Where("id = ? AND kind = ?", id, WebhookEmail).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &EmailLinkResult{}, nil // déjà retirée
	}
	if err != nil {
		return nil, err
	}
	if err := s.DB.WithContext(ctx).Where("id = ?", id).Delete(&webhookRow{}).Error; err != nil {
		return nil, err
	}
	return s.linkResult(ctx, row), nil
}
