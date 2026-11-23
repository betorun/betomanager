package ovhvps

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail/imapbox"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

var _ service.RescueProvider = (*Provider)(nil)

// SetRescue : démarrage sur le système de secours (rescue) ou sur le disque
// (local) au prochain redémarrage.
func (p *Provider) SetRescue(ctx context.Context, sn string, on bool) error {
	mode := "local"
	if on {
		mode = "rescue"
	}
	return p.api.Call(ctx, http.MethodPut, vpsPath(sn), map[string]any{"netbootMode": mode}, nil)
}

// Mailbox : e-mails du fournisseur reçus par la boîte technique du compte.
type Mailbox struct{ Box *imapbox.Box }

var _ service.RescueMailbox = (*Mailbox)(nil)

// FindRescue : identifiants du mode secours de ce VPS, dans un e-mail reçu
// depuis since ; nil si aucun.
func (m *Mailbox) FindRescue(ctx context.Context, providerID string, since time.Time) (*service.RescueMail, error) {
	msgs, err := m.Box.Search(ctx, since, providerID)
	if err != nil {
		return nil, err
	}
	// Le plus récent d'abord : un mode secours redemandé remplace le précédent.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if creds, ok := ParseRescueMail(msg.Subject, msg.Text, providerID); ok {
			return &service.RescueMail{ID: msg.UID, Credentials: creds}, nil
		}
	}
	return nil, nil
}

func (m *Mailbox) Delete(ctx context.Context, ids ...uint32) error { return m.Box.Delete(ctx, ids...) }
func (m *Mailbox) Check(ctx context.Context) (int, error)          { return m.Box.Check(ctx) }

var (
	rescueHint = regexp.MustCompile(`(?i)\b(rescue|secours)\b`)
	passwordRe = regexp.MustCompile(`(?im)(?:password|mot de passe)[^:=\n]{0,40}[:=]\s*([^\s<>"']{6,128})`)
	userRe     = regexp.MustCompile(`(?im)(?:user(?:name)?|login|identifiant|utilisateur|nom d'utilisateur)[^:=\n]{0,20}[:=]\s*([a-z_][a-z0-9_-]{0,31})\b`)
)

// IsRescueMail : e-mail de mode secours porteur d'un mot de passe, quel que
// soit le VPS cité (même critère que ParseRescueMail : sujet ou texte). Un
// tel e-mail n'est jamais stocké en clair pour l'équipe.
func IsRescueMail(subject, text string) bool {
	return rescueHint.MatchString(subject+"\n"+text) && passwordRe.MatchString(text)
}

// ParseRescueMail : e-mail de mode secours qui cite ce VPS → identifiants
// (utilisateur root par défaut). Tolérant aux variantes FR/EN et au HTML.
func ParseRescueMail(subject, text, providerID string) (domain.RescueCredentials, bool) {
	all := subject + "\n" + text
	if providerID == "" || !strings.Contains(all, providerID) || !rescueHint.MatchString(all) {
		return domain.RescueCredentials{}, false
	}
	m := passwordRe.FindStringSubmatch(text)
	if m == nil {
		return domain.RescueCredentials{}, false
	}
	creds := domain.RescueCredentials{Username: "root", Password: strings.TrimRight(m[1], ".,;")}
	if u := userRe.FindStringSubmatch(text); u != nil {
		creds.Username = u[1]
	}
	return creds, true
}
