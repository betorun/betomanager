package service

import (
	"bytes"
	"fmt"
	"net/mail"
	"net/smtp"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"go.uber.org/zap"
)

var templateRoot = resolveTemplateRoot()

var sendMail = smtp.SendMail

type MailerService struct {
	cfg    configs.MailConfig
	logger *zap.SugaredLogger
	auth   smtp.Auth
}

func NewMailerService(cfg configs.MailConfig, logger *zap.SugaredLogger) *MailerService {
	var auth smtp.Auth
	if cfg.SMTPUser != "" || cfg.SMTPPassword != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)
	}
	return &MailerService{
		cfg:    cfg,
		logger: logger,
		auth:   auth,
	}
}

func (m *MailerService) GenerateHTML(templateName string, data map[string]string) (string, error) {
	base := "base.html"
	// Version traduite (« en/<gabarit> », cf. LocalizedTemplate) : son propre
	// cadre si elle en a un ; sans traduction, le gabarit français.
	if dir, name := path.Split(templateName); dir != "" {
		if fileExists(templateName) {
			if fileExists(dir + "base.html") {
				base = dir + "base.html"
			}
		} else {
			templateName = name
		}
	}
	tmpl, err := template.ParseFS(os.DirFS(templateRoot), base, templateName)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", templateName, err)
	}

	var body bytes.Buffer
	if err := tmpl.ExecuteTemplate(&body, "base", data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return body.String(), nil
}

func (m *MailerService) SendEmail(to []string, subject string, htmlBody string) error {
	if !m.cfg.Enabled {
		m.logger.Infow("Mail service disabled (would send)", "to", to, "template", "HTML Content")
		return nil
	}
	if len(to) == 0 {
		return fmt.Errorf("at least one recipient is required")
	}

	m.logger.Debugw("Email Body Debug", "body", htmlBody)

	headers := make(map[string]string)
	headers["From"] = formatFromHeader(m.cfg.FromName, m.cfg.FromEmail)
	headers["To"] = to[0]
	headers["Subject"] = subject
	headers["MIME-Version"] = "1.0"
	headers["Content-Type"] = "text/html; charset=\"UTF-8\""

	message := ""
	for k, v := range headers {
		message += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	message += "\r\n" + htmlBody

	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)

	err := sendMail(addr, m.auth, m.cfg.FromEmail, to, []byte(message))
	if err != nil {
		return err
	}

	return nil
}

func resolveTemplateRoot() string {
	if custom := os.Getenv("MAIL_TEMPLATE_DIR"); custom != "" {
		if dirExists(custom) {
			return custom
		}
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if ok {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "templates"))
		if dirExists(candidate) {
			return candidate
		}
	}

	candidates := []string{
		"mail/templates",
		"/root/mail/templates",
	}
	for _, c := range candidates {
		if dirExists(c) {
			return c
		}
	}

	return "mail/templates"
}

// LocalizedTemplate : nom du gabarit à rendre pour une langue. Le français
// est la langue des gabarits d'origine ; les autres vivent dans un sous-dossier
// (« en/welcome.html »). GenerateHTML retombe sur le français s'il manque.
func LocalizedTemplate(templateName, locale string) string {
	if locale == "" || locale == "fr" || strings.Contains(templateName, "/") {
		return templateName
	}
	return locale + "/" + templateName
}

func fileExists(name string) bool {
	info, err := os.Stat(filepath.Join(templateRoot, filepath.FromSlash(name)))
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func formatFromHeader(name string, email string) string {
	if email == "" {
		return ""
	}
	if name == "" {
		return email
	}
	return (&mail.Address{Name: name, Address: email}).String()
}
