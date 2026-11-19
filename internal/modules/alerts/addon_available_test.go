package alerts

import (
	"context"
	"strings"
	"testing"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

func TestAddonAvailableEmail(t *testing.T) {
	m := &fakeMail{}
	n := &Notifier{Mail: m, ConsoleURL: "https://console.test/"}
	if err := n.AddonAvailable(context.Background(), i18n.EN, "awa@x.test", "Awa", "MariaDB"); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 1 {
		t.Fatalf("sent %d", len(m.sent))
	}
	msg := m.sent[0]
	if msg.To != "awa@x.test" || msg.Subject != "MariaDB is available on betorun" {
		t.Fatalf("to/subject = %q / %q", msg.To, msg.Subject)
	}
	for _, want := range []string{"Hello Awa,", "https://console.test/services", "Notify me"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("text lacks %q:\n%s", want, msg.Text)
		}
	}
	if strings.Contains(msg.Text+msg.HTML, "{{") {
		t.Fatal("unfilled template in email")
	}

	m.sent = nil
	if err := n.AddonAvailable(context.Background(), i18n.FR, "b@x.test", "", "MariaDB"); err != nil {
		t.Fatal(err)
	}
	if m.sent[0].Subject != "MariaDB est disponible sur betorun" || !strings.Contains(m.sent[0].Text, "Bonjour b,") {
		t.Fatalf("fr email: %q\n%s", m.sent[0].Subject, m.sent[0].Text)
	}
}
