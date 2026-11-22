package mail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSMTP : serveur SMTP minimal qui capture le message reçu.
func fakeSMTP(t *testing.T) (port int, received chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	received = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tp := textproto.NewConn(conn)
		_ = tp.PrintfLine("220 fake")
		var data strings.Builder
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				_ = tp.PrintfLine("250 fake")
			case strings.HasPrefix(line, "DATA"):
				_ = tp.PrintfLine("354 go")
				r := bufio.NewReader(tp.DotReader())
				b, _ := io.ReadAll(r)
				data.Write(b)
				_ = tp.PrintfLine("250 ok")
				received <- data.String()
			case strings.HasPrefix(line, "QUIT"):
				_ = tp.PrintfLine("221 bye")
				return
			default:
				_ = tp.PrintfLine("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, received
}

func TestSMTP_SendMultipart(t *testing.T) {
	port, received := fakeSMTP(t)
	s, err := NewSMTP(Config{Host: "127.0.0.1", Port: port, FromEmail: "no-reply@betorun.app", FromName: "betorun"})
	require.NoError(t, err)
	require.NoError(t, s.Send(context.Background(), Message{To: "awa@ex.sn", Subject: "Vérifiez votre adresse",
		Text: "Bonjour Awa, cliquez : https://console.betorun.app/verify-email?token=abc", HTML: "<p>Bonjour Awa</p>"}))

	msg := <-received
	assert.Contains(t, msg, `From: "betorun" <no-reply@betorun.app>`)
	assert.Contains(t, msg, "To: <awa@ex.sn>")
	dec := new(mime.WordDecoder)
	subjectLine := msg[strings.Index(msg, "Subject: ")+len("Subject: "):]
	subject, err := dec.DecodeHeader(subjectLine[:strings.Index(subjectLine, "\n")])
	require.NoError(t, err)
	assert.Equal(t, "Vérifiez votre adresse", subject, "objet encodé (accents)")
	assert.Contains(t, msg, "multipart/alternative")
	assert.Contains(t, msg, "text/plain; charset=utf-8")
	assert.Contains(t, msg, "text/html; charset=utf-8")
	assert.NotContains(t, strings.ToLower(msg), "keycloak")
}

func TestNewSMTP_RequiresConfig(t *testing.T) {
	_, err := NewSMTP(Config{Host: "smtp", Port: 0, FromEmail: "x@y"})
	assert.Error(t, err)
}

// Pièce jointe et Reply-To : multipart/mixed lisible par un client de mail.
func TestSMTP_AttachmentAndReplyTo(t *testing.T) {
	s, err := NewSMTP(Config{Host: "127.0.0.1", Port: 25, FromEmail: "no-reply@betorun.app"})
	require.NoError(t, err)
	to := &mail.Address{Address: "awa@ex.sn"}
	raw, err := s.compose(to, Message{To: "awa@ex.sn", Subject: "Facture", Text: "Votre facture", HTML: "<p>Votre facture</p>",
		ReplyTo: "support@betorun.app", Attachments: []Attachment{{Filename: "BTR-2026-0001.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7 fake")}}})
	require.NoError(t, err)

	m, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, "<support@betorun.app>", m.Header.Get("Reply-To"))
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	require.NoError(t, err)
	assert.Equal(t, "multipart/mixed", mt)
	r := multipart.NewReader(m.Body, params["boundary"])
	body, err := r.NextPart()
	require.NoError(t, err)
	assert.Contains(t, body.Header.Get("Content-Type"), "multipart/alternative")
	att, err := r.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "BTR-2026-0001.pdf", att.FileName())
	data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, att))
	require.NoError(t, err)
	assert.Equal(t, "%PDF-1.7 fake", string(data))

	_, err = s.compose(to, Message{To: "awa@ex.sn", Attachments: []Attachment{{Filename: "x\r\nBcc: evil@x", Data: []byte("x")}}})
	assert.True(t, IsInvalid(err), "en-tête injecté par un nom de fichier")
	_, err = s.compose(to, Message{To: "awa@ex.sn", Attachments: []Attachment{{Filename: "big.bin", Data: make([]byte, MaxAttachmentsBytes+1)}}})
	assert.ErrorIs(t, err, ErrInvalidAttachment)
	_, err = s.compose(to, Message{To: "awa@ex.sn", ReplyTo: "pas une adresse"})
	assert.ErrorIs(t, err, ErrInvalidReplyTo)
}
