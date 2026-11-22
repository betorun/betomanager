
func (m *MailerService) SendEmailWithAttachments(to []string, subject, html string, attachments []mailcontract.Attachment) error {
	if len(attachments) == 0 {
		return m.SendEmail(to, subject, html)
	}
	if !m.cfg.Enabled {
		return nil
	}
	if len(to) == 0 {
		return fmt.Errorf("at least one recipient is required")
	}
	for _, value := range append(append([]string{subject, m.cfg.FromEmail, m.cfg.FromName}, to...), attachmentNames(attachments)...) {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid mail header")
		}
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/html; charset=UTF-8"}, "Content-Transfer-Encoding": {"base64"}})
	if err != nil {
		return err
	}
	if _, err = part.Write(mimeBase64([]byte(html))); err != nil {
		return err
	}
	for _, a := range attachments {
		if len(a.Data) == 0 || a.Filename == "" {
			return fmt.Errorf("empty attachment")
		}
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			return err
		}
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {contentType},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return err
		}
		if _, err = part.Write(mimeBase64(a.Data)); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	header := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", formatFromHeader(m.cfg.FromName, m.cfg.FromEmail), strings.Join(to, ", "), mime.QEncoding.Encode("UTF-8", subject), writer.Boundary())
	return sendMail(fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort), m.auth, m.cfg.FromEmail, to, append([]byte(header), body.Bytes()...))
}
func attachmentNames(attachments []mailcontract.Attachment) []string {
	names := make([]string, 0, len(attachments)*2)
	for _, a := range attachments {
		names = append(names, a.Filename, a.ContentType)
	}
	return names
}
func mimeBase64(data []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(data)
	var out bytes.Buffer
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded + "\r\n")
	return out.Bytes()
}
