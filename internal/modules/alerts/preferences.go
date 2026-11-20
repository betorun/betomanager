package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Préférences par personne : langue des e-mails et catégories d'alertes de
// projet coupées. Les e-mails de sécurité, d'accès et de facturation ne se
// coupent pas (ADR 0009).

// Catégories d'alertes de projet que l'on peut couper.
const (
	CategoryWorkloads = "containers" // conteneur en incident
	CategoryBuilds    = "builds"     // build échoué
	CategoryDisk      = "disk"       // disque presque plein, plein, arrêt
	CategoryAPIKeys   = "api_keys"   // nouvelle clé API d'un compte de service
)

// Categories : dans l'ordre d'affichage.
var Categories = []string{CategoryWorkloads, CategoryBuilds, CategoryDisk, CategoryAPIKeys}

// categoryOf : catégorie d'un type d'alerte ("" : ne se coupe pas).
func categoryOf(kind string) string {
	switch {
	case kind == KindWorkloadIncident:
		return CategoryWorkloads
	case kind == KindBuildFailed:
		return CategoryBuilds
	case strings.HasPrefix(kind, KindDisk):
		return CategoryDisk
	case kind == KindAPIKeyCreated:
		return CategoryAPIKeys
	}
	return ""
}

func validCategory(c string) bool {
	for _, x := range Categories {
		if x == c {
			return true
		}
	}
	return false
}

// Erreurs des préférences.
var (
	ErrUnknownCategory = errors.New("unknown alert category")
	ErrInvalidLocale   = errors.New("unsupported language")
	ErrInvalidToken    = errors.New("this unsubscribe link is invalid")
)

// Preferences : réponse et requête de /identity/me/preferences.
type Preferences struct {
	// Locale : "fr", "en" ou "" (pas encore choisie).
	Locale string `json:"locale"`
	// Alerts : catégorie → reçue (true) ou coupée (false).
	Alerts map[string]bool `json:"alerts"`
}

// PreferencesUpdate : champs absents inchangés.
type PreferencesUpdate struct {
	Locale *string         `json:"locale,omitempty" description:"Langue des e-mails : fr ou en"`
	Alerts map[string]bool `json:"alerts,omitempty" description:"Catégorie → reçue (true) ou coupée (false) : containers, builds, disk, api_keys"`
}

// PreferenceStore : préférences en base.
type PreferenceStore struct{ DB *gorm.DB }

type muteRow struct {
	UserID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	Category string    `gorm:"primaryKey"`
}

func (muteRow) TableName() string { return "alert_mutes" }

// Get : préférences d'une personne.
func (s *PreferenceStore) Get(ctx context.Context, user uuid.UUID) (*Preferences, error) {
	out := &Preferences{Alerts: map[string]bool{}}
	if err := s.DB.WithContext(ctx).Raw(`SELECT locale FROM users WHERE id = ?`, user).Scan(&out.Locale).Error; err != nil {
		return nil, err
	}
	var muted []string
	if err := s.DB.WithContext(ctx).Model(&muteRow{}).Where("user_id = ?", user).Pluck("category", &muted).Error; err != nil {
		return nil, err
	}
	for _, c := range Categories {
		out.Alerts[c] = true
	}
	for _, c := range muted {
		if validCategory(c) {
			out.Alerts[c] = false
		}
	}
	return out, nil
}

// Update : applique les changements demandés.
func (s *PreferenceStore) Update(ctx context.Context, user uuid.UUID, in PreferencesUpdate) (*Preferences, error) {
	for c := range in.Alerts {
		if !validCategory(c) {
			return nil, ErrUnknownCategory
		}
	}
	if in.Locale != nil && *in.Locale != "fr" && *in.Locale != "en" {
		return nil, ErrInvalidLocale
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if in.Locale != nil {
			if err := tx.Exec(`UPDATE users SET locale = ? WHERE id = ?`, *in.Locale, user).Error; err != nil {
				return err
			}
		}
		for c, on := range in.Alerts {
			if err := setMuted(tx, user, c, !on); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, user)
}

func setMuted(tx *gorm.DB, user uuid.UUID, category string, muted bool) error {
	if muted {
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&muteRow{UserID: user, Category: category}).Error
	}
	return tx.Where("user_id = ? AND category = ?", user, category).Delete(&muteRow{}).Error
}

// Mute : coupe une catégorie (lien de désabonnement).
func (s *PreferenceStore) Mute(ctx context.Context, user uuid.UUID, category string) error {
	if !validCategory(category) {
		return ErrUnknownCategory
	}
	return setMuted(s.DB.WithContext(ctx), user, category, true)
}

// ── Lien de désabonnement ────────────────────────────────────────────────

// UnsubscribeToken : jeton signé « cette personne, cette catégorie ». Sans
// date d'expiration : un lien de désabonnement doit marcher longtemps, et il
// ne fait que couper des e-mails.
func UnsubscribeToken(key []byte, user uuid.UUID, category string) string {
	payload := user.String() + "." + category
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sign(key, payload))
}

// ParseUnsubscribeToken : personne et catégorie d'un jeton valide.
func ParseUnsubscribeToken(key []byte, token string) (uuid.UUID, string, error) {
	if len(key) == 0 {
		return uuid.Nil, "", ErrInvalidToken
	}
	p, s, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return uuid.Nil, "", ErrInvalidToken
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, sign(key, string(raw))) {
		return uuid.Nil, "", ErrInvalidToken
	}
	id, category, ok := strings.Cut(string(raw), ".")
	user, err := uuid.Parse(id)
	if !ok || err != nil || !validCategory(category) {
		return uuid.Nil, "", ErrInvalidToken
	}
	return user, category, nil
}

func sign(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("alerts-unsubscribe\x00" + payload))
	return m.Sum(nil)[:16]
}

// unsubscribeLinks : page de la console et URL « un clic » de l'API (vide
// si l'URL publique de l'API n'est pas configurée).
func (n *Notifier) unsubscribeLinks(user uuid.UUID, category string) (page, oneClick string) {
	if len(n.UnsubscribeKey) == 0 || user == uuid.Nil || category == "" {
		return "", ""
	}
	tok := url.QueryEscape(UnsubscribeToken(n.UnsubscribeKey, user, category))
	page = strings.TrimRight(n.ConsoleURL, "/") + "/unsubscribe?token=" + tok
	if n.APIURL != "" {
		oneClick = strings.TrimRight(n.APIURL, "/") + "/alerts/unsubscribe?token=" + tok
	}
	return page, oneClick
}

// withoutMuted : retire ceux qui ont coupé la catégorie (en cas d'erreur de
// lecture, tout le monde reçoit l'alerte).
func (n *Notifier) withoutMuted(ctx context.Context, rs []Recipient, category string) []Recipient {
	ids := make([]uuid.UUID, 0, len(rs))
	for _, r := range rs {
		if r.ID != uuid.Nil {
			ids = append(ids, r.ID)
		}
	}
	muted, err := n.Store.Muted(ctx, category, ids)
	if err != nil {
		n.log().Warnw("alert mutes lookup failed", "category", category, "error", err)
		return rs
	}
	out := rs[:0:0]
	for _, r := range rs {
		if !muted[r.ID] {
			out = append(out, r)
		}
	}
	return out
}
