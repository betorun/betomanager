package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Carte des fonctionnalités construite depuis la VRAIE spécification :
// questions de débutant (français, sans accents, pluriels), et fonctionnalités
// qui n'existent pas, pour lesquelles la réponse doit être « NO MATCH ».
func TestCopilotFeatureMap(t *testing.T) {
	spec, err := publicSpec(documentedAPI(t).Generator().API(), i18n.EN)
	require.NoError(t, err)
	cat, err := tools.Build(CopilotRoutes(AllGroups), spec)
	require.NoError(t, err)
	require.NoError(t, tools.AddDiagnose(cat))
	fr, guides := CopilotFeatureInput(AllGroups)
	require.NoError(t, tools.AddFeatureMap(cat, spec, fr, guides, CopilotScreens()))

	find := func(q string) string {
		args, _ := json.Marshal(map[string]string{"query": q})
		r := cat.Call(context.Background(), tools.Invoker{}, tools.ToolFindFeatures, args)
		require.False(t, r.IsError, r.Content)
		return r.Content
	}

	out := find("créer une base de données")
	assert.Contains(t, out, "action card: create_database")

	out = find("supprimer ma base")
	assert.Contains(t, out, "done by the person in the console: /services")
	assert.Contains(t, out, "needs two-factor authentication")

	out = find("reseau ip de sortie")
	assert.Contains(t, out, "request_egress_ip")
	assert.Contains(t, out, "/network")

	out = find("factures")
	assert.Contains(t, out, "list_billing_documents")

	out = find("prix")
	assert.Contains(t, out, "get_price_catalog")

	out = find("deployer depuis github")
	assert.Contains(t, out, "create_workload_from_github")

	out = find("mot de passe")
	assert.Contains(t, out, "/settings")

	for _, q := range []string{"serverless lambda", "GPU", "kubernetes helm chart", "machine learning notebook"} {
		assert.True(t, strings.HasPrefix(find(q), "NO MATCH"), q)
	}

	out = find("")
	assert.Contains(t, out, "Areas of betorun")
	assert.Contains(t, out, "/network:")

	// Noyau : chaque outil du noyau existe (un nom faux le ferait disparaître
	// sans bruit), et il reste petit.
	core := cat.Core()
	require.Len(t, core, len(tools.CoreTools))
	require.LessOrEqual(t, len(core), 30)
	// Ce que find_features nomme existe dans le catalogue.
	for _, n := range tools.NamedTools(find("base de donnees conteneur domaine vps facture")) {
		_, ok := cat.Lookup(n)
		require.True(t, ok, n)
	}

	def, ok := cat.Lookup(tools.ToolFindFeatures)
	require.True(t, ok)
	assert.Contains(t, def.Description, "never invent")
}
