package mail

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// Un serveur qui accepte la connexion mais ne répond jamais : l'annulation du
// contexte doit interrompre l'envoi, sans attendre le délai de la config.
func TestSend_HonorsContextCancel(t *testing.T) {
	defer goleak.VerifyNone(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	s, err := NewSMTP(Config{Host: "127.0.0.1", Port: port, FromEmail: "noreply@example.com", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err = s.Send(ctx, Message{To: "a@example.com", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatal("Send succeeded against a silent server")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Send ignored cancellation: returned after %v", d)
	}
	if c := <-accepted; c != nil {
		c.Close()
	}
}
