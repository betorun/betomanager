package services

import (
	"bytes"
	"context"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

// Emails transactionnels de betorun, en français et en anglais (langue de
// la requête). Les liens mènent aux pages de la console (Next.js).

// Pages de la console qui reçoivent les liens.
const (
	pageVerifyEmail   = "/verify-email"
	pageResetPassword = "/reset-password"
	pageSignIn        = "/sign-in"
	pageForgot        = "/forgot-password"
	pageSecurity      = "/settings#sessions"
)

type emailKind int

const (
	emailVerify emailKind = iota
	emailReset
	emailAccountExists
	emailPasswordChanged
	// Sécurité du compte : prévenir le titulaire d'un changement qu'il n'a
	// peut-être pas fait.
	emailNewDevice
	emailMFAEnabled
	emailMFADisabled
	emailRecoveryRegenerated
	emailRecoveryUsed
	emailIdentityLinked
	emailPasskeyAdded
	emailPasskeyRemoved
)

// securityOutro : fin commune des e-mails de sécurité.
var securityOutro = map[i18n.Locale]string{
	i18n.FR: "Si ce n'était pas vous, changez votre mot de passe tout de suite et fermez les sessions que vous ne reconnaissez pas.",
	i18n.EN: "If this wasn't you, change your password right away and sign out the sessions you don't recognize.",
}

// emailCopy : textes d'un email dans une langue.
type emailCopy struct {
	Subject, Title, Intro, Button, Outro string
}

var emailCopies = map[emailKind]map[i18n.Locale]emailCopy{
	emailNewDevice: {
		i18n.FR: {Subject: "Nouvelle connexion à votre compte betorun", Title: "Nouvelle connexion",
			Intro:  "Bonjour {{.Name}}, votre compte vient d'être utilisé depuis un appareil que nous ne connaissions pas : {{.Device}}, adresse IP {{.IP}}, {{.When}}.",
			Button: "Voir mes sessions", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "New sign-in to your betorun account", Title: "New sign-in",
			Intro:  "Hello {{.Name}}, your account was just used from a device we didn't know: {{.Device}}, IP address {{.IP}}, {{.When}}.",
			Button: "View my sessions", Outro: securityOutro[i18n.EN]},
	},
	emailMFAEnabled: {
		i18n.FR: {Subject: "Double authentification activée", Title: "Double authentification activée",
			Intro:  "Bonjour {{.Name}}, la double authentification est maintenant active sur votre compte betorun. Gardez vos codes de secours en lieu sûr : ils servent si vous perdez votre téléphone.",
			Button: "Voir la sécurité du compte", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "Two-step verification turned on", Title: "Two-step verification turned on",
			Intro:  "Hello {{.Name}}, two-step verification is now on for your betorun account. Keep your recovery codes somewhere safe: you need them if you lose your phone.",
			Button: "View account security", Outro: securityOutro[i18n.EN]},
	},
	emailMFADisabled: {
		i18n.FR: {Subject: "Double authentification désactivée", Title: "Double authentification désactivée",
			Intro:  "Bonjour {{.Name}}, la double authentification vient d'être désactivée sur votre compte betorun. Votre mot de passe suffit désormais pour vous connecter.",
			Button: "La réactiver", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "Two-step verification turned off", Title: "Two-step verification turned off",
			Intro:  "Hello {{.Name}}, two-step verification was just turned off for your betorun account. Your password alone is now enough to sign in.",
			Button: "Turn it back on", Outro: securityOutro[i18n.EN]},
	},
	emailRecoveryRegenerated: {
		i18n.FR: {Subject: "Nouveaux codes de secours", Title: "Nouveaux codes de secours",
			Intro:  "Bonjour {{.Name}}, de nouveaux codes de secours ont été créés pour votre compte betorun. Les anciens ne fonctionnent plus.",
			Button: "Voir la sécurité du compte", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "New recovery codes", Title: "New recovery codes",
			Intro:  "Hello {{.Name}}, new recovery codes were created for your betorun account. The old ones no longer work.",
			Button: "View account security", Outro: securityOutro[i18n.EN]},
	},
	emailRecoveryUsed: {
		i18n.FR: {Subject: "Un code de secours a été utilisé", Title: "Code de secours utilisé",
			Intro:  "Bonjour {{.Name}}, un code de secours vient d'être utilisé pour vous connecter ({{.Device}}, adresse IP {{.IP}}, {{.When}}). Chaque code ne sert qu'une fois. S'il vous en reste peu, créez-en de nouveaux.",
			Button: "Voir la sécurité du compte", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "A recovery code was used", Title: "Recovery code used",
			Intro:  "Hello {{.Name}}, a recovery code was just used to sign in ({{.Device}}, IP address {{.IP}}, {{.When}}). Each code works only once. If you have few left, create new ones.",
			Button: "View account security", Outro: securityOutro[i18n.EN]},
	},
	emailIdentityLinked: {
		i18n.FR: {Subject: "Compte {{.Provider}} lié à votre compte betorun", Title: "Nouveau moyen de connexion",
			Intro:  "Bonjour {{.Name}}, un compte {{.Provider}} ({{.Email}}) vient d'être lié à votre compte betorun : il permet désormais de vous connecter.",
			Button: "Voir mes moyens de connexion", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "{{.Provider}} account linked to your betorun account", Title: "New sign-in method",
			Intro:  "Hello {{.Name}}, a {{.Provider}} account ({{.Email}}) was just linked to your betorun account: it can now be used to sign in.",
			Button: "View my sign-in methods", Outro: securityOutro[i18n.EN]},
	},
	emailPasskeyAdded: {
		i18n.FR: {Subject: "Nouvelle passkey sur votre compte betorun", Title: "Nouvelle passkey",
			Intro:  "Bonjour {{.Name}}, une passkey ({{.Passkey}}) vient d'être ajoutée à votre compte betorun : elle permet de vous connecter et de confirmer les actions sensibles.",
			Button: "Voir la sécurité du compte", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "New passkey on your betorun account", Title: "New passkey",
			Intro:  "Hello {{.Name}}, a passkey ({{.Passkey}}) was just added to your betorun account: it can be used to sign in and to confirm sensitive actions.",
			Button: "View account security", Outro: securityOutro[i18n.EN]},
	},
	emailPasskeyRemoved: {
		i18n.FR: {Subject: "Passkey retirée de votre compte betorun", Title: "Passkey retirée",
			Intro:  "Bonjour {{.Name}}, une passkey vient d'être retirée de votre compte betorun.",
			Button: "Voir la sécurité du compte", Outro: securityOutro[i18n.FR]},
		i18n.EN: {Subject: "Passkey removed from your betorun account", Title: "Passkey removed",
			Intro:  "Hello {{.Name}}, a passkey was just removed from your betorun account.",
			Button: "View account security", Outro: securityOutro[i18n.EN]},
	},
	emailVerify: {
		i18n.FR: {Subject: "Confirmez votre adresse email", Title: "Bienvenue sur betorun",
			Intro:  "Bonjour {{.Name}}, confirmez votre adresse email pour activer votre compte.",
			Button: "Confirmer mon adresse", Outro: "Ce lien est valable 24 heures. Si vous n'avez pas créé de compte, ignorez cet email."},
		i18n.EN: {Subject: "Confirm your email address", Title: "Welcome to betorun",
			Intro:  "Hello {{.Name}}, confirm your email address to activate your account.",
			Button: "Confirm my address", Outro: "This link is valid for 24 hours. If you did not create an account, ignore this email."},
	},
	emailReset: {
		i18n.FR: {Subject: "Réinitialisez votre mot de passe", Title: "Mot de passe oublié",
			Intro:  "Bonjour {{.Name}}, choisissez un nouveau mot de passe pour votre compte betorun.",
			Button: "Choisir un nouveau mot de passe", Outro: "Ce lien est valable 1 heure et ne sert qu'une fois. Si vous n'êtes pas à l'origine de cette demande, ignorez cet email : votre mot de passe ne change pas."},
		i18n.EN: {Subject: "Reset your password", Title: "Forgot your password?",
			Intro:  "Hello {{.Name}}, choose a new password for your betorun account.",
			Button: "Choose a new password", Outro: "This link is valid for 1 hour and works only once. If you did not request this, ignore this email: your password stays the same."},
	},
	emailAccountExists: {
		i18n.FR: {Subject: "Vous avez déjà un compte betorun", Title: "Vous avez déjà un compte",
			Intro:  "Bonjour, quelqu'un (vous, sans doute) a tenté de créer un compte betorun avec cette adresse, qui en a déjà un.",
			Button: "Me connecter", Outro: "Mot de passe oublié ? Réinitialisez-le depuis la page de connexion. Si ce n'était pas vous, ignorez cet email."},
		i18n.EN: {Subject: "You already have a betorun account", Title: "You already have an account",
			Intro:  "Hello, someone (probably you) tried to create a betorun account with this address, which already has one.",
			Button: "Sign in", Outro: "Forgot your password? Reset it from the sign-in page. If this wasn't you, ignore this email."},
	},
	emailPasswordChanged: {
		i18n.FR: {Subject: "Votre mot de passe a été modifié", Title: "Mot de passe modifié",
			Intro:  "Bonjour {{.Name}}, le mot de passe de votre compte betorun vient d'être modifié. Vos autres sessions ont été fermées.",
			Button: "Me connecter", Outro: "Si vous n'êtes pas à l'origine de ce changement, réinitialisez votre mot de passe immédiatement."},
		i18n.EN: {Subject: "Your password was changed", Title: "Password changed",
			Intro:  "Hello {{.Name}}, the password of your betorun account was just changed. Your other sessions were signed out.",
			Button: "Sign in", Outro: "If you did not make this change, reset your password immediately."},
	},
}

var textLayout = texttemplate.Must(texttemplate.New("text").Parse(`{{.Title}}

{{.Intro}}

{{.Button}} : {{.Link}}

{{.Outro}}

— betorun
`))

var htmlLayout = htmltemplate.Must(htmltemplate.New("html").Parse(`<!doctype html>
<html lang="{{.Lang}}"><body style="margin:0;padding:24px;background:#f6f4f2;font-family:'IBM Plex Sans',Arial,sans-serif;color:#1d1a19">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:8px;padding:32px">
<tr><td style="font-size:20px;font-weight:600;color:#7A1F3D;padding-bottom:16px">betorun</td></tr>
<tr><td style="font-size:18px;font-weight:600;padding-bottom:12px">{{.Title}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;padding-bottom:24px">{{.Intro}}</td></tr>
<tr><td style="padding-bottom:24px"><a href="{{.Link}}" style="display:inline-block;background:#7A1F3D;color:#ffffff;text-decoration:none;padding:12px 20px;border-radius:6px;font-weight:600">{{.Button}}</a></td></tr>
<tr><td style="font-size:13px;line-height:1.5;color:#6b6461">{{.Outro}}</td></tr>
</table></td></tr></table></body></html>`))

// renderEmail : email prêt à envoyer, dans la langue de ctx.
func renderEmail(ctx context.Context, kind emailKind, to, name, link string) (mail.Message, error) {
	return renderEmailWith(ctx, kind, to, name, link, nil)
}

// renderEmailWith : vars complète le texte (Device, IP, When, Provider, Email) ;
// le sujet est rempli lui aussi.
func renderEmailWith(ctx context.Context, kind emailKind, to, name, link string, vars map[string]string) (mail.Message, error) {
	lang := i18n.From(ctx)
	if lang != i18n.EN {
		lang = i18n.FR
	}
	copy := emailCopies[kind][lang]
	values := map[string]string{"Name": firstNonBlank(name, strings.Split(to, "@")[0])}
	for k, v := range vars {
		values[k] = v
	}
	intro, err := fill(copy.Intro, values)
	if err != nil {
		return mail.Message{}, err
	}
	subject, err := fill(copy.Subject, values)
	if err != nil {
		return mail.Message{}, err
	}
	copy.Subject = subject
	data := map[string]string{"Lang": string(lang), "Title": copy.Title, "Intro": intro, "Button": copy.Button,
		"Outro": copy.Outro, "Link": link}
	var text, html bytes.Buffer
	if err := textLayout.Execute(&text, data); err != nil {
		return mail.Message{}, err
	}
	if err := htmlLayout.Execute(&html, data); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{To: to, Subject: copy.Subject, Text: text.String(), HTML: html.String()}, nil
}

func fill(tpl string, data map[string]string) (string, error) {
	t, err := texttemplate.New("intro").Parse(tpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// consoleLink : lien vers une page de la console, avec un jeton éventuel.
func (s *service) consoleLink(page, token string) string {
	base := strings.TrimRight(s.frontendBaseURL, "/")
	if token == "" {
		return base + page
	}
	return fmt.Sprintf("%s%s?token=%s", base, page, url.QueryEscape(token))
}

func firstNonBlank(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
