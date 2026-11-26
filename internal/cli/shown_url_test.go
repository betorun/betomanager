package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/cli/api"
)

// La CLI montre le domaine du client quand l'API en annonce un, sinon
// l'adresse de la plateforme (API plus ancienne, ou aucun domaine vérifié).
func TestShownURL(t *testing.T) {
	sslip := "https://site-dc9111.51.75.141.233.sslip.io"
	assert.Equal(t, "https://ecole-demo.sykedu.com", shownURL(&api.Workload{ExternalURL: sslip, PrimaryURL: "https://ecole-demo.sykedu.com"}))
	assert.Equal(t, sslip, shownURL(&api.Workload{ExternalURL: sslip}))
	assert.Empty(t, shownURL(&api.Workload{}))
}
