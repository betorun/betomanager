package router

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Les outils du Copilot se construisent depuis la VRAIE spécification : une
// route renommée, déplacée ou dont un paramètre n'est plus documenté casse
// ce test au lieu de casser le Copilot en production.
func TestCopilotToolsBuildFromPublicSpec(t *testing.T) {
	spec, err := publicSpec(documentedAPI(t).Generator().API(), i18n.EN)
	require.NoError(t, err)
	routes := CopilotRoutes(AllGroups)
	require.GreaterOrEqual(t, len(routes), 15)

	cat, err := tools.Build(routes, spec)
	require.NoError(t, err)
	require.NoError(t, tools.AddDiagnose(cat))

	defs := cat.Definitions()
	require.Len(t, defs, len(routes)+1)
	for _, d := range defs {
		require.NotEmpty(t, d.Description, d.Name)
		require.True(t, strings.HasPrefix(string(d.Parameters), `{`), d.Name)
		// Descriptions en anglais : aucune lettre accentuée française.
		require.NotContains(t, d.Description, "é", d.Name)
	}
	logs, ok := cat.Lookup("get_workload_logs")
	require.True(t, ok)
	require.NotContains(t, string(logs.Schema), `"follow"`, "follow est imposé, pas exposé")
	require.Contains(t, string(logs.Schema), `"maximum":300`)
}

// Aucune route marquée ForCopilot n'est une écriture (lot 1 : lecture seule).
func TestCopilotToolsAreReadOnly(t *testing.T) {
	documentedAPI(t)
	for _, r := range CopilotRoutes(AllGroups) {
		require.Equal(t, "GET", r.Method, r.Name)
	}
}
