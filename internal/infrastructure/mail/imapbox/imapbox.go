// Package imapbox : lecture d'une boîte e-mail technique par IMAP (TLS).
// Utilisé par le mode compute pour recevoir les e-mails que le fournisseur
// des VPS envoie au compte (identifiants du mode secours) : ils n'arrivent
// jamais chez le client, et leur contenu n'est jamais journalisé.
package imapbox

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

// Config : boîte technique (secrets hors du code).
type Config struct {
	Host     string
	Port     int // défaut 993
	Username string
	Password string
	Mailbox  string // défaut INBOX
}

// ErrIncomplete : un réglage manque.
var ErrIncomplete = errors.New("imap: incomplete mailbox settings")

// Message : un e-mail, texte seulement (HTML réduit à son texte).
type Message struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time
	Text    string
}

// Box : connexion ouverte à chaque appel (appels rares, pas de session longue).
type Box struct {
	cfg     Config
	timeout time.Duration
}

// New : erreur si un réglage manque.
func New(cfg Config) (*Box, error) {
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, ErrIncomplete
	}
	if cfg.Port == 0 {
		cfg.Port = 993
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	return &Box{cfg: cfg, timeout: 30 * time.Second}, nil
}

func (b *Box) open(ctx context.Context) (*client.Client, error) {
	addr := net.JoinHostPort(b.cfg.Host, strconv.Itoa(b.cfg.Port))
	d := &net.Dialer{Timeout: b.timeout}
	if dl, ok := ctx.Deadline(); ok {
		d.Deadline = dl
	}
	c, err := client.DialWithDialerTLS(d, addr, &tls.Config{ServerName: b.cfg.Host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	c.Timeout = b.timeout
	if err := c.Login(b.cfg.Username, b.cfg.Password); err != nil {
		_ = c.Logout()
		return nil, err
	}
	if _, err := c.Select(b.cfg.Mailbox, false); err != nil {
		_ = c.Logout()
		return nil, err
	}
	return c, nil
}

// Check : la boîte répond ; rend le nombre de messages.
func (b *Box) Check(ctx context.Context) (int, error) {
	c, err := b.open(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Logout() }()
	return int(c.Mailbox().Messages), nil
}

// maxMessages : messages lus au plus par recherche (les plus récents) ;
// maxFetch : octets lus au plus par message (pièces jointes tronquées).
const (
	maxMessages = 100
	maxFetch    = 512 << 10
)

// Search : messages reçus depuis since (au jour près côté serveur, puis à la
// date du message) qui contiennent text ; les maxMessages plus récents, chacun
// lu sur ses maxFetch premiers octets (une boîte pleine ne gonfle pas la mémoire).
func (b *Box) Search(ctx context.Context, since time.Time, text string) ([]Message, error) {
	c, err := b.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Logout() }()
	crit := imap.NewSearchCriteria()
	crit.Since = since.Add(-24 * time.Hour)
	if text != "" {
		crit.Text = []string{text}
	}
	uids, err := c.UidSearch(crit)
	if err != nil || len(uids) == 0 {
		return nil, err
	}
	if len(uids) > maxMessages {
		// Les UID croissent avec l'arrivée : les derniers sont les plus récents.
		slices.Sort(uids)
		uids = uids[len(uids)-maxMessages:]
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, maxFetch}}
	ch := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, section.FetchItem()}, ch)
	}()
	var out []Message
	for m := range ch {
		if m == nil || m.Envelope == nil {
			continue
		}
		if !m.Envelope.Date.IsZero() && m.Envelope.Date.Before(since) {
			continue
		}
		msg := Message{UID: m.Uid, Subject: m.Envelope.Subject, Date: m.Envelope.Date}
		if len(m.Envelope.From) > 0 {
			msg.From = m.Envelope.From[0].Address()
		}
		if r := m.GetBody(section); r != nil {
			msg.Text = readText(r)
		}
		out = append(out, msg)
	}
	return out, <-done
}

// Delete : les messages sont effacés (marqués puis purgés), en une session.
func (b *Box) Delete(ctx context.Context, uids ...uint32) error {
	if len(uids) == 0 {
		return nil
	}
	c, err := b.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Logout() }()
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	if err := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	return c.Expunge(nil)
}

const maxText = 256 << 10

var (
	tagRe   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	spaceRe = regexp.MustCompile(`[ \t\r]+`)
)

// readText : parties texte (et HTML réduit à son texte) du message.
func readText(r io.Reader) string {
	mr, err := mail.CreateReader(r)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for b.Len() < maxText {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		h, ok := p.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		ct, _, _ := h.ContentType()
		raw, _ := io.ReadAll(io.LimitReader(p.Body, maxText))
		switch {
		case strings.HasPrefix(ct, "text/plain"):
			b.Write(raw)
		case strings.HasPrefix(ct, "text/html"):
			b.WriteString(HTMLText(string(raw)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// HTMLText : texte lisible d'un HTML simple (balises retirées, entités courantes).
func HTMLText(html string) string {
	s := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n", "</tr>", "\n", "</div>", "\n", "</li>", "\n").Replace(html)
	s = tagRe.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	return spaceRe.ReplaceAllString(s, " ")
}
