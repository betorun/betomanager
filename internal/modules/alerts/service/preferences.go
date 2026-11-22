package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
)

// Préférences par personne : langue des e-mails et catégories d'alertes de
// projet coupées. Les e-mails de sécurité, d'accès et de facturation ne se
// coupent pas (ADR 0009).

// categoryOf : catégorie d'un type d'alerte ("" : ne se coupe pas).
func categoryOf(kind string) string {
	switch {
	case kind == KindWorkloadIncident:
		return domain.CategoryWorkloads
	case kind == KindBuildFailed:
		return domain.CategoryBuilds
	case strings.HasPrefix(kind, KindDisk):
		return domain.CategoryDisk
	case kind == KindAPIKeyCreated:
		return domain.CategoryAPIKeys
	}
	return ""
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
		return uuid.Nil, "", domain.ErrInvalidToken
	}
	p, s, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return uuid.Nil, "", domain.ErrInvalidToken
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, sign(key, string(raw))) {
		return uuid.Nil, "", domain.ErrInvalidToken
	}
	id, category, ok := strings.Cut(string(raw), ".")
	user, err := uuid.Parse(id)
	if !ok || err != nil || !domain.ValidCategory(category) {
		return uuid.Nil, "", domain.ErrInvalidToken
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
func (n *Notifier) withoutMuted(ctx context.Context, rs []domain.Recipient, category string) []domain.Recipient {
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
