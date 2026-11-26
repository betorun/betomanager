package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

func TestPrimaryURL(t *testing.T) {
	sslip := "https://site-dc9111.51.75.141.233.sslip.io"
	for _, c := range []struct {
		name     string
		external string
		routes   []domain.Route
		want     string
	}{
		{"un domaine à la racine", sslip, []domain.Route{{Host: "ecole-demo.sykedu.com", Path: "/"}}, "https://ecole-demo.sykedu.com"},
		{"la racine passe avant un chemin", sslip,
			[]domain.Route{{Host: "a.example.org", Path: "/api"}, {Host: "z.example.org", Path: "/"}}, "https://z.example.org"},
		{"ordre stable entre deux racines", sslip,
			[]domain.Route{{Host: "www.b.org", Path: "/"}, {Host: "a.org", Path: "/"}}, "https://a.org"},
		{"chemin seul : gardé dans l'adresse", sslip, []domain.Route{{Host: "x.org", Path: "/docs"}}, "https://x.org/docs"},
		{"conteneur sans certificat : http", "http://site.1.2.3.4.sslip.io", []domain.Route{{Host: "x.org", Path: "/"}}, "http://x.org"},
		{"pas d'adresse de plateforme : https", "", []domain.Route{{Host: "x.org"}}, "https://x.org"},
	} {
		assert.Equal(t, c.want, primaryURL(c.external, c.routes), c.name)
	}
}
