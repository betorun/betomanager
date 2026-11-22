package contract

// MailEvent is the payload published to the async mail queue.
type MailEvent struct {
	Attachments []Attachment      `json:"attachments,omitempty"`
	To          string            `json:"to"`
	Subject     string            `json:"subject"`
	Template    string            `json:"template"`
	Variables   map[string]string `json:"variables"`
}

// Attachment contains immutable bytes; JSON transports Data as base64.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}
