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

// Lectures en GET ; écritures déclarées Write, jamais DELETE ni sensibles, avec
// un risque ; les secrets ne sont jamais un argument du modèle.
func TestCopilotToolsReadsAndWrites(t *testing.T) {
	documentedAPI(t)
	writes := 0
	for _, r := range CopilotRoutes(AllGroups) {
		if !r.Write {
			require.Equal(t, "GET", r.Method, r.Name)
			continue
		}
		writes++
		require.NotEqual(t, "DELETE", r.Method, r.Name)
		require.False(t, r.Sensitive, r.Name)
		require.NotEmpty(t, r.Risk, r.Name)
	}
	require.GreaterOrEqual(t, writes, 10)

	spec, err := publicSpec(documentedAPI(t).Generator().API(), i18n.EN)
	require.NoError(t, err)
	cat, err := tools.Build(CopilotRoutes(AllGroups), spec)
	require.NoError(t, err)
	vars, ok := cat.Lookup("update_workload_variables")
	require.True(t, ok)
	require.NotContains(t, string(vars.Schema), "secrets", "valeur secrète jamais confiée au modèle")
	require.Contains(t, string(vars.Schema), `"env"`)

	// Création de conteneur : liste blanche (ni secret_data, ni compte de
	// service, ni stockage, ni exécution en root).
	create, ok := cat.Lookup("create_workload")
	require.True(t, ok)
	for _, hidden := range []string{"secret_data", "service_account", "run_as_user", "persistence_enabled", "service_type"} {
		require.NotContains(t, string(create.Schema), `"`+hidden+`"`, hidden)
	}
	for _, shown := range []string{"project_id", "name", "image", "target_port", "env_vars"} {
		require.Contains(t, string(create.Schema), `"`+shown+`"`, shown)
	}
	_, ok = cat.Lookup("build_from_git")
	require.True(t, ok)
}
