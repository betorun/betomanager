package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/thekrauss/kubemanager/internal/infrastructure/certstatus"
	"github.com/thekrauss/kubemanager/internal/infrastructure/helm/charts"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// Le nom calculé ici doit être celui que pose le chart (secretName de
// l'Ingress, repris par cert-manager pour le Certificate).
func TestCertificateName_MatchesChart(t *testing.T) {
	c, err := charts.StandardApp()
	require.NoError(t, err)
	values := map[string]any{
		"ingress":         map[string]any{"enabled": false, "className": "traefik"},
		"customDomains":   []any{map[string]any{"host": "app.client.org", "path": "/"}, map[string]any{"host": "app.client.org", "path": "/auth"}},
		"customDomainTLS": true,
	}
	vals, err := chartutil.ToRenderValues(c, values, chartutil.ReleaseOptions{Name: "web", Namespace: "proj-1", IsInstall: true}, chartutil.DefaultCapabilities)
	require.NoError(t, err)
	out, err := engine.Render(c, vals)
	require.NoError(t, err)
	body := out["standard-app/templates/custom-domains.yaml"]
	for _, path := range []string{"/", "/auth"} {
		name := CertificateName("web", "app.client.org", path)
		assert.True(t, strings.Contains(body, "secretName: "+name), "%s absent du rendu :\n%s", name, body)
	}
}

func TestCertificateFor(t *testing.T) {
	d := &domain.WorkloadDomain{Hostname: "app.client.org", Path: "/", Verified: true}
	name := CertificateName("web", d.Hostname, d.Path)
	certs := map[string]*certstatus.Status{name: {State: certstatus.StateActive}}
	assert.Equal(t, certstatus.StateActive, certificateFor(certs, "web", d).State)
	assert.Equal(t, certstatus.StatePending, certificateFor(map[string]*certstatus.Status{}, "web", d).State, "pas encore créé : en cours")
	assert.Nil(t, certificateFor(nil, "web", d), "cluster injoignable : rien")
	d.Verified = false
	assert.Nil(t, certificateFor(certs, "web", d), "domaine non vérifié : rien")
}
