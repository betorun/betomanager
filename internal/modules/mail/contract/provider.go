package contract

// MailSender is the only interface other modules should depend on
// to dispatch outgoing emails (fire-and-forget, async via pub/sub).
type MailSender interface {
	SendAsync(to, subject, template string, vars map[string]string)
}
