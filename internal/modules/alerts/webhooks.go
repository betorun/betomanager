package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Webhooks d'alertes d'un projet : les alertes de projet (conteneurs, builds,
// disques, clés API) partent aussi vers Slack ou vers une URL qui reçoit un
// JSON signé (HMAC-SHA256, en-tête X-Betorun-Signature).

// Sortes de webhook.
const (
	WebhookSlack = "slack"
	WebhookJSON  = "json"
)

// MaxWebhooks : par projet.
const MaxWebhooks = 5

// Erreurs des webhooks.
var (
	ErrWebhookURL      = errors.New("the webhook address must be a public https:// URL")
	ErrWebhookKind     = errors.New("webhook type must be slack or json")
	ErrWebhookName     = errors.New("webhook name must be 1 to 60 characters")
	ErrWebhookLimit    = errors.New("this project already has the maximum number of webhooks")
	ErrWebhookNotFound = errors.New("webhook not found")
	ErrWebhookPrivate  = errors.New("refusing to call a private address")
)

// Sealer : chiffrement des URL et des secrets (auth.Sealer).
type Sealer interface {
	Seal(plain []byte, associated string) ([]byte, error)
	Open(sealed []byte, associated string) ([]byte, error)
}

// Webhook : tel que montré dans la console (jamais l'URL complète).
type Webhook struct {
	ID             uuid.UUID  `json:"id"`
	ProjectID      uuid.UUID  `json:"-"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	URLHint        string     `json:"url_hint"`
	Locale         string     `json:"locale"`
	CreatedAt      time.Time  `json:"created_at"`
	LastDeliveryAt *time.Time `json:"last_delivery_at,omitempty"`
	// LastStatus : « ok », « HTTP 404 », « unreachable »…
	LastStatus string `json:"last_status"`
}

type webhookRow struct {
	Webhook
	URLSealed    []byte
	SecretSealed []byte
	CreatedBy    string
}

func (webhookRow) TableName() string { return "alert_webhooks" }

// CreateWebhookRequest : nouveau webhook.
type CreateWebhookRequest struct {
	Name string `json:"name" binding:"required" description:"Nom affiché (1 à 60 caractères)"`
	Kind string `json:"kind" binding:"required" description:"slack ou json"`
	URL  string `json:"url" binding:"required" description:"Adresse https:// publique (URL de webhook entrant Slack, ou votre service)"`
}

// CreatedWebhook : le secret de signature (sorte json) n'est montré qu'ici.
type CreatedWebhook struct {
	Webhook
	Secret string `json:"secret,omitempty"`
}

// WebhookList : webhooks d'un projet.
type WebhookList struct {
	Webhooks []Webhook `json:"webhooks"`
	Max      int       `json:"max"`
}

// WebhookService : webhooks en base et envoi.
type WebhookService struct {
	DB     *gorm.DB
	Sealer Sealer
	Client *http.Client
	Now    func() time.Time
}

func (s *WebhookService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func sealContext(id uuid.UUID, what string) string {
	return "alert-webhook:" + id.String() + ":" + what
}

// validateWebhookURL : https, hôte public nommé ou IP publique, sans
// identifiants. La connexion vérifie de nouveau l'adresse (DNS).
func validateWebhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return nil, ErrWebhookURL
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && privateIP(ip) {
		return nil, ErrWebhookURL
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") {
		return nil, ErrWebhookURL
	}
	return u, nil
}

// hint : hôte et fin du chemin (« hooks.slack.com/…/AbCd »).
func hint(u *url.URL) string {
	path := strings.Trim(u.EscapedPath(), "/")
	if path == "" {
		return u.Host
	}
	tail := path
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return u.Host + "/…" + tail
}

// List : webhooks du projet.
func (s *WebhookService) List(ctx context.Context, project uuid.UUID) (*WebhookList, error) {
	var rows []webhookRow
	if err := s.DB.WithContext(ctx).Where("project_id = ?", project).Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := &WebhookList{Webhooks: make([]Webhook, 0, len(rows)), Max: MaxWebhooks}
	for _, r := range rows {
		out.Webhooks = append(out.Webhooks, r.Webhook)
	}
	return out, nil
}

// Create : URL et secret chiffrés ; langue des messages : celle de la requête.
func (s *WebhookService) Create(ctx context.Context, project uuid.UUID, by string, in CreateWebhookRequest) (*CreatedWebhook, error) {
	name := strings.TrimSpace(in.Name)
	if n := len([]rune(name)); n == 0 || n > 60 {
		return nil, ErrWebhookName
	}
	if in.Kind != WebhookSlack && in.Kind != WebhookJSON {
		return nil, ErrWebhookKind
	}
	u, err := validateWebhookURL(in.URL)
	if err != nil {
		return nil, err
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&webhookRow{}).Where("project_id = ?", project).Count(&count).Error; err != nil {
		return nil, err
	}
	if count >= MaxWebhooks {
		return nil, ErrWebhookLimit
	}
	locale := "fr"
	if i18n.From(ctx) == i18n.EN {
		locale = "en"
	}
	row := webhookRow{Webhook: Webhook{ID: uuid.New(), ProjectID: project, Name: name, Kind: in.Kind, URLHint: hint(u),
		Locale: locale, CreatedAt: s.now()}, CreatedBy: by}
	if row.URLSealed, err = s.Sealer.Seal([]byte(u.String()), sealContext(row.ID, "url")); err != nil {
		return nil, err
	}
	out := &CreatedWebhook{Webhook: row.Webhook}
	if in.Kind == WebhookJSON {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		out.Secret = "whsec_" + hex.EncodeToString(b)
		if row.SecretSealed, err = s.Sealer.Seal([]byte(out.Secret), sealContext(row.ID, "secret")); err != nil {
			return nil, err
		}
	}
	if err := s.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Delete : retire un webhook du projet.
func (s *WebhookService) Delete(ctx context.Context, project, id uuid.UUID) error {
	res := s.DB.WithContext(ctx).Where("project_id = ? AND id = ?", project, id).Delete(&webhookRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

// WebhookEvent : contenu commun d'une alerte envoyée aux webhooks.
type WebhookEvent struct {
	Type      string    `json:"type"`
	ProjectID uuid.UUID `json:"project_id"`
	Project   string    `json:"project"`
	SubjectID uuid.UUID `json:"subject_id"`
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	Link      string    `json:"link"`
	SentAt    time.Time `json:"sent_at"`
}

// Test : message d'essai ; renvoie l'état obtenu (« ok », « HTTP 404 »…).
func (s *WebhookService) Test(ctx context.Context, project, id uuid.UUID, projectName, consoleURL string) (*Webhook, error) {
	var row webhookRow
	if err := s.DB.WithContext(ctx).Where("project_id = ? AND id = ?", project, id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	copies := testCopies[i18n.FR]
	if row.Locale == "en" {
		copies = testCopies[i18n.EN]
	}
	ev := WebhookEvent{Type: "test", ProjectID: project, Project: projectName, SubjectID: id, Title: copies[0],
		Text: strings.ReplaceAll(copies[1], "{p}", projectName), Link: strings.TrimRight(consoleURL, "/") + "/dashboard", SentAt: s.now()}
	s.deliver(ctx, row, ev)
	if err := s.DB.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return nil, err
	}
	return &row.Webhook, nil
}

var testCopies = map[i18n.Locale][2]string{
	i18n.FR: {"Test des alertes betorun", "Ce webhook recevra les alertes du projet « {p} »."},
	i18n.EN: {"betorun alert test", "This webhook will receive the alerts of project \"{p}\"."},
}

// Send : une alerte vers tous les webhooks du projet ; copies : textes par
// langue (déjà remplis).
func (s *WebhookService) Send(ctx context.Context, project uuid.UUID, kind string, subject uuid.UUID, projectName, link string,
	texts map[i18n.Locale][2]string) {
	var rows []webhookRow
	if err := s.DB.WithContext(ctx).Where("project_id = ?", project).Find(&rows).Error; err != nil {
		return
	}
	for _, row := range rows {
		t := texts[i18n.FR]
		if row.Locale == "en" {
			t = texts[i18n.EN]
		}
		s.deliver(ctx, row, WebhookEvent{Type: kind, ProjectID: project, Project: projectName, SubjectID: subject,
			Title: t[0], Text: t[1], Link: link, SentAt: s.now()})
	}
}

// deliver : un envoi, état enregistré (jamais l'URL ni la réponse).
func (s *WebhookService) deliver(ctx context.Context, row webhookRow, ev WebhookEvent) {
	status := s.post(ctx, row, ev)
	now := s.now()
	_ = s.DB.WithContext(ctx).Model(&webhookRow{}).Where("id = ?", row.ID).
		Updates(map[string]any{"last_delivery_at": now, "last_status": status}).Error
}

func (s *WebhookService) post(ctx context.Context, row webhookRow, ev WebhookEvent) string {
	rawURL, err := s.Sealer.Open(row.URLSealed, sealContext(row.ID, "url"))
	if err != nil {
		return "error"
	}
	var body []byte
	if row.Kind == WebhookSlack {
		body, _ = json.Marshal(map[string]string{"text": fmt.Sprintf("*%s*\n%s\n<%s|%s>", slackEscape(ev.Title),
			slackEscape(ev.Text), ev.Link, slackEscape(ev.Project))})
	} else {
		body, _ = json.Marshal(ev)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(rawURL), bytes.NewReader(body))
	if err != nil {
		return "error"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "betorun-alerts/1")
	if row.Kind == WebhookJSON && len(row.SecretSealed) > 0 {
		secret, err := s.Sealer.Open(row.SecretSealed, sealContext(row.ID, "secret"))
		if err != nil {
			return "error"
		}
		ts := strconv.FormatInt(ev.SentAt.Unix(), 10)
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(ts + "." + string(body)))
		req.Header.Set("X-Betorun-Timestamp", ts)
		req.Header.Set("X-Betorun-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	}
	client := s.Client
	if client == nil {
		client = PublicHTTPClient()
	}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrWebhookPrivate) || strings.Contains(err.Error(), ErrWebhookPrivate.Error()) {
			return "private address refused"
		}
		return "unreachable"
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return "ok"
	}
	return "HTTP " + strconv.Itoa(res.StatusCode)
}

// slackEscape : caractères de contrôle de Slack (&, <, >).
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// PublicHTTPClient : pas de redirection, adresses internes refusées au moment
// de la connexion (une résolution DNS ne peut pas le contourner).
func PublicHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refusePrivateDial}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			Proxy: nil, MaxIdleConns: 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip)
}

func refusePrivateDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || privateIP(ip) {
		return ErrWebhookPrivate
	}
	return nil
}

// webhookTexts : titre (sujet de l'e-mail) et texte par langue.
func webhookTexts(copies map[i18n.Locale]alertCopy, project, name string) map[i18n.Locale][2]string {
	out := map[i18n.Locale][2]string{}
	vars := map[string]string{"Name": name, "Project": project}
	for lang, c := range copies {
		title, err1 := fill(c.Subject, vars)
		body, err2 := fill(c.Body, vars)
		if err1 == nil && err2 == nil {
			out[lang] = [2]string{title, body}
		}
	}
	return out
}
