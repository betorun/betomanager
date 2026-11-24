package operator

import (
	"os/exec"
	"strings"
	"testing"
)

// L'Opérateur démarre quand l'API est en panne : il n'embarque ni le
// routeur, ni les modules, ni la configuration, ni l'IAM de l'API, et ne
// lui parle que par HTTP (ADR 0016 §1 bis).
func TestNoAPIDependencies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go absent")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/thekrauss/kubemanager/cmd/operator").CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	forbidden := []string{
		"kubemanager/internal/modules/",
		"kubemanager/internal/infrastructure/router",
		"kubemanager/internal/infrastructure/database",
		"kubemanager/internal/core/configs",
		"kubemanager/internal/iam",
		"kubemanager/internal/auth",
		"kubemanager/internal/middleware",
		"gorm.io/",
		"github.com/gin-gonic/",
		"go.temporal.io/",
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, f := range forbidden {
			if strings.Contains(line, f) {
				t.Errorf("dépendance interdite dans l'Opérateur : %s", line)
			}
		}
	}
}
