package services

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// E-mails de sécurité du compte : envoyés au titulaire, hors du chemin de la
// requête (un SMTP lent ne retarde pas la connexion), au mieux.

// securityMailTimeout : envoi abandonné au-delà.
const securityMailTimeout = 30 * time.Second

// securityMail : e-mail de sécurité au titulaire du compte userID.
func (s *service) securityMail(ctx context.Context, userID uuid.UUID, kind emailKind, link string, vars map[string]string) {
	if s.mailer == nil || s.users == nil || s.frontendBaseURL == "" {
		return
	}
	// Langue et valeurs de la requête gardées, annulation non.
	bg := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(bg, securityMailTimeout)
		defer cancel()
		user, err := s.users.GetUserByID(ctx, userID)
		if err != nil || user == nil || user.Email == "" {
			return
		}
		msg, err := renderEmailWith(ctx, kind, user.Email, user.FullName, link, vars)
		if err == nil {
			err = s.mailer.Send(ctx, msg)
		}
		if err != nil && s.logger != nil {
			s.logger.Errorw("security email failed", "kind", kind, zap.Error(err))
		}
	}()
}

// whenText : date et heure de l'événement, en UTC.
func whenText(ctx context.Context, t time.Time) string {
	t = t.UTC()
	if i18n.From(ctx) == i18n.EN {
		return "on " + t.Format("Jan 2, 2006 at 15:04") + " UTC"
	}
	return "le " + t.Format("02/01/2006 à 15:04") + " UTC"
}

func ipText(ip string) string {
	if ip == "" {
		return "?"
	}
	return ip
}

// noticeLogin : après une connexion réussie. Appareil inconnu (hors toute
// première connexion du compte) ou code de secours : le titulaire est prévenu.
func (s *service) noticeLogin(ctx context.Context, userID uuid.UUID, kind, method string, cl Client) {
	label := ""
	newDevice := false
	if s.devices != nil {
		seen, err := s.devices.Seen(ctx, userID, kind, cl.UserAgent)
		if err != nil {
			if s.logger != nil {
				s.logger.Warnw("device lookup failed", zap.Error(err))
			}
		} else {
			label, newDevice = seen.Label, seen.New && !seen.First
		}
	}
	if label == "" {
		_, label = auth.DescribeDevice(kind, cl.UserAgent)
	}
	vars := map[string]string{"Device": label, "IP": ipText(cl.IP), "When": whenText(ctx, time.Now())}
	link := s.consoleLink(pageSecurity, "")
	switch {
	case method == auth.MethodRecovery:
		// Code de secours : cet e-mail dit aussi l'appareil.
		s.securityMail(ctx, userID, emailRecoveryUsed, link, vars)
	case newDevice:
		s.securityMail(ctx, userID, emailNewDevice, link, vars)
	}
}

// providerTitle : nom affiché d'un fournisseur de connexion.
func providerTitle(provider string) string {
	switch provider {
	case "google":
		return "Google"
	case "github":
		return "GitHub"
	}
	return provider
}
