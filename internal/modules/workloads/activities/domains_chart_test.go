package activities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildHelmValues_CustomDomains(t *testing.T) {
	// Entrée d'un workflow antérieur aux chemins : tout le domaine.
	v := buildHelmValues(InstallWorkloadInput{ReleaseName: "api", CustomDomains: []string{"a.sn", "b.sn"}, CustomDomainTLS: true})
	assert.Equal(t, []any{map[string]any{"host": "a.sn", "path": "/"}, map[string]any{"host": "b.sn", "path": "/"}}, v["customDomains"])
	routes := buildHelmValues(InstallWorkloadInput{ReleaseName: "api", CustomRoutes: []DomainRoute{{Host: "a.sn", Path: "/auth"}}, CustomDomains: []string{"ignoré.sn"}})
	assert.Equal(t, []any{map[string]any{"host": "a.sn", "path": "/auth"}}, routes["customDomains"])
	assert.Equal(t, true, v["customDomainTLS"])
	assert.Empty(t, buildHelmValues(InstallWorkloadInput{ReleaseName: "api"})["customDomains"])
}

// Une limite basse ne doit pas échouer à cause de la réservation par défaut.
func TestRequestWithin(t *testing.T) {
	assert.Equal(t, "64Mi", requestWithin("", "128Mi", "64Mi"))
	assert.Equal(t, "128Mi", requestWithin("", "128Mi", "512Mi"))
	assert.Equal(t, "128Mi", requestWithin("", "128Mi", ""))
	assert.Equal(t, "32Mi", requestWithin("32Mi", "128Mi", "64Mi"), "valeur explicite conservée")
	assert.Equal(t, "50m", requestWithin("", "100m", "50m"))
}
