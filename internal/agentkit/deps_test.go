package agentkit

import (
	"os/exec"
	"strings"
	"testing"
)

// La boucle commune ne connaît aucun produit : ni modules, ni routeur, ni
// configuration de l'API (l'Opérateur l'embarque hors de l'API).
func TestNoProductDependencies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go absent")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/thekrauss/kubemanager/internal/agentkit").CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	for _, forbidden := range []string{"internal/modules/", "internal/infrastructure/router", "internal/core/configs", "gorm.io/", "github.com/gin-gonic/"} {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, forbidden) {
				t.Errorf("dépendance interdite dans agentkit : %s", line)
			}
		}
	}
}
